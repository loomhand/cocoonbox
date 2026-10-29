// Package connecttest serves a mock vendor for Connect tests: an MCP server behind MCP authorization, with
// dynamic registration, a client metadata document or a client registered by hand, PKCE, rotating refresh tokens and revocation.
package connecttest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// Key is the header key the vendor accepts in X-Api-Key.
	Key = "mock-api-key"

	accessTTL = 3600
)

// Call is one tools/call the server ran.
type Call struct {
	Tool      string
	Arguments map[string]any
	Auth      string
}

type codeGrant struct {
	challenge   string
	redirectURI string
	clientID    string
}

// Vendor is the mock vendor; its fields count what clients did.
type Vendor struct {
	*httptest.Server
	// CIMD makes the authorization server take client metadata documents instead of registration.
	CIMD bool
	// SSE answers tools/call as an event stream.
	SSE bool
	// Delay holds each refresh, so concurrent refreshes overlap.
	Delay time.Duration
	// Refusal answers tools/list with this HTTP 403 body, as a server that does not admit the account.
	Refusal string
	// NoResourceMetadata serves no protected resource metadata, as a server of MCP authorization 2025-03-26.
	NoResourceMetadata bool
	// Iss names the authorization server in every authorization response (RFC 9207) and says so in its metadata.
	Iss bool
	// ClientID and ClientSecret are a client registered by hand: no registration, and the token endpoint requires the secret.
	ClientID     string
	ClientSecret string

	mu        sync.Mutex
	codes     map[string]codeGrant
	access    map[string]bool
	refresh   map[string]bool
	clients   map[string]bool
	refreshes int
	revoked   []string
	calls     []Call
	rejected  bool
}

// New starts a vendor.
func New() *Vendor {
	v := &Vendor{codes: map[string]codeGrant{}, access: map[string]bool{}, refresh: map[string]bool{}, clients: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", v.mcp)
	mux.HandleFunc("/keyed", v.keyed)
	mux.HandleFunc("/open", v.open)
	mux.HandleFunc("GET /userinfo", v.userinfo)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", v.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", v.serverMetadata)
	mux.HandleFunc("POST /register", v.register)
	mux.HandleFunc("GET /authorize", v.authorize)
	mux.HandleFunc("POST /token", v.token)
	mux.HandleFunc("POST /revoke", v.revoke)
	v.Server = httptest.NewServer(mux)
	return v
}

// MCPURL is the OAuth-protected MCP endpoint.
func (v *Vendor) MCPURL() string { return v.URL + "/mcp" }

// KeyedURL is the MCP endpoint that takes Key in X-Api-Key.
func (v *Vendor) KeyedURL() string { return v.URL + "/keyed" }

// OpenURL is the MCP endpoint that needs no credential.
func (v *Vendor) OpenURL() string { return v.URL + "/open" }

// Consent answers an authorization URL the way a user's consent does, and returns the code and state it redirects with.
func (v *Vendor) Consent(authorizeURL string) (string, string, error) {
	answer, err := v.Answer(authorizeURL)
	return answer.Get("code"), answer.Get("state"), err
}

// Answer answers an authorization URL the way a user's consent does, and returns every parameter it redirects with.
func (v *Vendor) Answer(authorizeURL string) (url.Values, error) {
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(authorizeURL)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return nil, err
	}
	return loc.Query(), nil
}

// ExpireAccess makes every access token invalid, as an expiry at the vendor does.
func (v *Vendor) ExpireAccess() {
	v.mu.Lock()
	defer v.mu.Unlock()
	clear(v.access)
}

// RejectRefresh makes every refresh answer invalid_grant.
func (v *Vendor) RejectRefresh() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.rejected = true
}

// Snapshot returns the counters under the lock.
func (v *Vendor) Snapshot() (refreshes int, revoked []string, calls []Call) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.refreshes, append([]string(nil), v.revoked...), append([]Call(nil), v.calls...)
}

func (v *Vendor) resourceMetadata(w http.ResponseWriter, _ *http.Request) {
	if v.NoResourceMetadata {
		http.NotFound(w, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resource": v.MCPURL(), "authorization_servers": []string{v.URL}, "scopes_supported": []string{"items:read", "items:write"}})
}

func (v *Vendor) serverMetadata(w http.ResponseWriter, _ *http.Request) {
	md := map[string]any{
		"issuer":                                v.URL,
		"authorization_endpoint":                v.URL + "/authorize",
		"token_endpoint":                        v.URL + "/token",
		"registration_endpoint":                 v.URL + "/register",
		"revocation_endpoint":                   v.URL + "/revoke",
		"code_challenge_methods_supported":      []string{"S256"},
		"client_id_metadata_document_supported": v.CIMD,
	}
	if v.Iss {
		md["authorization_response_iss_parameter_supported"] = true
	}
	if v.ClientID != "" {
		delete(md, "registration_endpoint")
	}
	writeJSON(w, http.StatusOK, md)
}

func (v *Vendor) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
	}
	if json.UnmarshalRead(r.Body, &req) != nil || len(req.RedirectURIs) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	id := "dcr-" + token()
	v.mu.Lock()
	v.clients[id] = true
	v.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"client_id": id, "token_endpoint_auth_method": "none"})
}

func (v *Vendor) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v.mu.Lock()
	known := v.clients[q.Get("client_id")] || (v.CIMD && strings.HasSuffix(q.Get("client_id"), "/cocoonbox.connect/client.json")) ||
		(v.ClientID != "" && q.Get("client_id") == v.ClientID)
	v.mu.Unlock()
	if !known || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("resource") != v.MCPURL() {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := token()
	v.mu.Lock()
	v.codes[code] = codeGrant{challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri"), clientID: q.Get("client_id")}
	v.mu.Unlock()
	answer := url.Values{"code": {code}, "state": {q.Get("state")}}
	if v.Iss {
		answer.Set("iss", v.URL)
	}
	http.Redirect(w, r, q.Get("redirect_uri")+"?"+answer.Encode(), http.StatusFound)
}

func (v *Vendor) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	if f.Get("resource") != v.MCPURL() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_target"})
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ClientID != "" && (f.Get("client_id") != v.ClientID || f.Get("client_secret") != v.ClientSecret) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		g, ok := v.codes[f.Get("code")]
		delete(v.codes, f.Get("code"))
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if !ok || g.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) || g.redirectURI != f.Get("redirect_uri") || g.clientID != f.Get("client_id") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
	case "refresh_token":
		if v.Delay > 0 {
			v.mu.Unlock()
			time.Sleep(v.Delay)
			v.mu.Lock()
		}
		v.refreshes++
		if v.rejected || !v.refresh[f.Get("refresh_token")] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		delete(v.refresh, f.Get("refresh_token"))
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	access, refresh := "at-"+token(), "rt-"+token()
	v.access[access], v.refresh[refresh] = true, true
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "refresh_token": refresh, "expires_in": accessTTL, "token_type": "Bearer", "scope": "items:read items:write"})
}

func (v *Vendor) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	v.mu.Lock()
	defer v.mu.Unlock()
	t := r.PostForm.Get("token")
	v.revoked = append(v.revoked, t)
	delete(v.refresh, t)
	delete(v.access, t)
	w.WriteHeader(http.StatusOK)
}

func (v *Vendor) mcp(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	v.mu.Lock()
	ok := v.access[strings.TrimPrefix(auth, "Bearer ")]
	v.mu.Unlock()
	if !ok {
		challenge := `Bearer resource_metadata="` + v.URL + `/.well-known/oauth-protected-resource/mcp", scope="items:read items:write"`
		if v.NoResourceMetadata {
			challenge = `Bearer realm="OAuth"`
		}
		w.Header().Set("WWW-Authenticate", challenge)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	v.serveMCP(w, r, auth)
}

func (v *Vendor) userinfo(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	ok := v.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	v.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"login": "mock-user"})
}

func (v *Vendor) keyed(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Api-Key") != Key {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	v.serveMCP(w, r, "X-Api-Key")
}

func (v *Vendor) open(w http.ResponseWriter, r *http.Request) {
	v.serveMCP(w, r, r.Header.Get("Authorization"))
}

func (v *Vendor) serveMCP(w http.ResponseWriter, r *http.Request, auth string) {
	var req struct {
		ID     jsontext.Value `json:"id"`
		Method string         `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if json.UnmarshalRead(r.Body, &req) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "s-"+token())
		result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "mock"}}
	case "tools/list":
		if v.Refusal != "" {
			http.Error(w, v.Refusal, http.StatusForbidden)
			return
		}
		result = map[string]any{"tools": []any{
			map[string]any{"name": "list_items", "description": "List items.", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}},
			map[string]any{"name": "create_item", "description": "Create an item.", "inputSchema": map[string]any{"type": "object"}},
		}}
	case "tools/call":
		v.mu.Lock()
		v.calls = append(v.calls, Call{Tool: req.Params.Name, Arguments: req.Params.Arguments, Auth: auth})
		v.mu.Unlock()
		if req.Params.Arguments["fail"] == "500" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": req.Params.Name + " done"}}}
	default:
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "no such method"}})
		return
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}, json.Deterministic(true))
	if v.SSE && req.Method == "tools/call" {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\nevent: message\ndata: " + string(body) + "\n\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, v, json.Deterministic(true))
}

func token() string { return strings.ToLower(rand.Text()) }
