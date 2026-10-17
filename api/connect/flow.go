package connect

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/net/http/httpguts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Started is a sign-in in progress: the vendor's authorization URL and the nonce its browser must return with.
type Started struct {
	URL       string
	Connector string
	Nonce     string
}

// StartRequest begins connecting an account for the console session whose secret hashes to SessionSHA256.
type StartRequest struct {
	User          string
	SessionSHA256 string
	Connector     string
	// Name and URL describe a custom connector; with Key it is stored at once and sent in Header (default Authorization).
	Name   string
	URL    string
	Header string
	Key    string
}

// Start stores a pending authorization; a connector without a sign-in, or a custom one with a key, connects at once and returns no URL.
func (s *Service) Start(ctx context.Context, req StartRequest) (*Started, error) {
	e := s.entry(req.Connector)
	switch {
	case e == nil:
		return nil, status.Errorf(codes.InvalidArgument, "unknown connector %q", req.Connector)
	case !e.available():
		return nil, status.Errorf(codes.FailedPrecondition, "%s is not set up on this server", e.name)
	}
	connector, mcpURL := e.id, e.mcpURL
	switch e.auth {
	case authCustom:
		var err error
		if connector, mcpURL, err = s.customTarget(req); err != nil {
			return nil, err
		}
		if req.Key != "" {
			return &Started{Connector: connector}, s.connectDirect(ctx, req.User, connector, e.id, mcpURL, credential{header: cmp.Or(req.Header, headerAuthorization), value: req.Key})
		}
	case authOpen:
		return &Started{Connector: connector}, s.connectDirect(ctx, req.User, connector, e.id, mcpURL, credential{})
	}
	verifier, challenge := pkce()
	state, nonce := randomToken(), randomToken()
	fs := flowSecrets{Verifier: verifier, Scopes: e.scopes}
	q := url.Values{
		"response_type":         {responseCode},
		"redirect_uri":          {s.publicURL + CallbackPath},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	authorizeURL := e.authorizeURL
	if e.auth == authOAuth {
		q.Set("client_id", e.client.ID)
		maps.Copy(q, e.params)
	} else {
		discoverCtx, cancel := context.WithTimeout(ctx, setupTimeout)
		auth, err := s.discover(discoverCtx, mcpURL, e.client)
		cancel()
		if errors.Is(err, errNoSignIn) {
			return &Started{Connector: connector}, s.connectDirect(ctx, req.User, connector, e.id, mcpURL, credential{})
		}
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "sign-in to %s: %v", mcpURL, err)
		}
		authorizeURL, fs.Client, fs.Issuer, fs.IssRequired = auth.authorizeURL, auth.client, auth.issuer, auth.issRequired
		if len(fs.Scopes) == 0 {
			fs.Scopes = auth.scopes
		}
		q.Set("client_id", auth.client.ID)
		q.Set("resource", auth.client.Resource)
	}
	if len(fs.Scopes) > 0 {
		q.Set("scope", strings.Join(fs.Scopes, " "))
	}
	target, err := url.Parse(authorizeURL)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "authorization endpoint %q: %v", authorizeURL, err)
	}
	merged := target.Query()
	maps.Copy(merged, q)
	target.RawQuery = merged.Encode()
	f := &flow{
		id: "cf_" + randomHex(idBytes), user: req.User, connector: connector, kind: e.id, mcpURL: mcpURL,
		stateSHA256: hashHex(state), nonceSHA256: hashHex(nonce), sessionSHA256: req.SessionSHA256, expiresAt: time.Now().Add(flowTTL),
	}
	if f.sealed, err = s.seal.seal(ctx, f.user, fs, flowData(f.id, f.user, f.connector)); err != nil {
		return nil, err
	}
	if err := s.store.insertFlow(ctx, f); err != nil {
		return nil, err
	}
	log.WithFunc("connect.Service.Start").Infof(ctx, "user %s began connecting %s (flow %s)", req.User, connector, f.id)
	return &Started{URL: target.String(), Connector: connector, Nonce: nonce}, nil
}

// Complete exchanges code for the flow of state whose browser holds nonce, while the console session that began it
// is live and iss, the answer's RFC 9207 issuer, names the flow's authorization server, and returns the connector id.
func (s *Service) Complete(ctx context.Context, nonce, state, code, iss string) (string, error) {
	f, err := s.store.takeFlow(ctx, hashHex(state), hashHex(nonce))
	if errors.Is(err, errNoFlow) {
		return "", status.Error(codes.PermissionDenied, err.Error())
	}
	if err != nil {
		return "", err
	}
	user := f.user
	if live, sessErr := s.sessions.SessionUser(ctx, f.sessionSHA256); sessErr != nil || live != user {
		return "", status.Error(codes.PermissionDenied, "the console session that began the sign-in ended; sign in and start again")
	}
	var fs flowSecrets
	if err = s.seal.open(ctx, user, f.sealed, flowData(f.id, f.user, f.connector), &fs); err != nil {
		return "", err
	}
	if !answeredBy(fs, iss) {
		log.WithFunc("connect.Service.Complete").Warnf(ctx, "refused the sign-in answer of flow %s: iss %q, the flow's issuer %q", f.id, iss, fs.Issuer)
		return "", status.Error(codes.PermissionDenied, "the sign-in answer came from another server than the one it began at; start again")
	}
	e := s.entry(f.kind)
	if e == nil {
		return "", status.Errorf(codes.FailedPrecondition, "connector kind %q left the catalog", f.kind)
	}
	c := fs.Client
	if e.auth == authOAuth {
		c = e.client
	}
	t, err := s.token(ctx, c, url.Values{
		"grant_type": {grantAuthorizationCode}, responseCode: {code}, "redirect_uri": {s.publicURL + CallbackPath}, "code_verifier": {fs.Verifier},
	})
	if err != nil {
		return "", status.Errorf(codes.FailedPrecondition, "%s refused the sign-in: %v", e.name, err)
	}
	account, _ := s.accountLabel(ctx, e, t.AccessToken)
	g := &grant{user: user, connector: f.connector, kind: f.kind, mcpURL: f.mcpURL, account: account, scopes: fs.Scopes, expiresAt: expiry(t.ExpiresIn)}
	if t.Scope != "" {
		g.scopes = strings.FieldsFunc(t.Scope, func(r rune) bool { return r == ' ' || r == ',' })
	}
	sec := secrets{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken}
	if e.auth != authOAuth {
		sec.Client = fs.Client
	}
	if err := s.store.putGrant(ctx, g, func(id string) ([]byte, error) {
		return s.seal.seal(ctx, user, sec, grantData(id, user, g.connector))
	}); err != nil {
		return "", err
	}
	s.dropConn(g.id)
	s.changed(ctx, g)
	log.WithFunc("connect.Service.Complete").Infof(ctx, "user %s connected %s as %q (grant %s, refresh token %t)", user, g.connector, g.account, g.id, t.RefreshToken != "")
	return g.connector, nil
}

// ServeClientMetadata serves the api's OAuth client metadata document; its URL is the client id.
func (s *Service) ServeClientMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"client_id":                  s.publicURL + ClientMetadataPath,
		"client_name":                "CocoonBox",
		"client_uri":                 s.publicURL,
		"redirect_uris":              []string{s.publicURL + CallbackPath},
		"grant_types":                []string{grantAuthorizationCode, grantRefreshToken},
		"response_types":             []string{responseCode},
		"token_endpoint_auth_method": authNone,
	})
}

func (s *Service) customTarget(req StartRequest) (string, string, error) {
	if !connectorPattern.MatchString(req.Name) || s.entry(req.Name) != nil {
		return "", "", status.Error(codes.InvalidArgument, "a custom connector's name is 1 to 24 lowercase letters, digits or -, starting with a letter, and not a catalog name")
	}
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && (u.Scheme != "http" || !s.allowPrivate)) {
		return "", "", status.Error(codes.InvalidArgument, "a custom connector's URL is an https URL")
	}
	if req.Key != "" && (!httpguts.ValidHeaderFieldName(cmp.Or(req.Header, headerAuthorization)) || !httpguts.ValidHeaderFieldValue(req.Key)) {
		return "", "", status.Error(codes.InvalidArgument, "the header name or the key is not valid in an HTTP header")
	}
	u.Fragment = ""
	return req.Name, u.String(), nil
}

// connectDirect stores a grant without a sign-in, with cred or none, only after the server listed its tools with it.
func (s *Service) connectDirect(ctx context.Context, user, connector, kind, mcpURL string, cred credential) error {
	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	probe := &upstream{s: s, url: mcpURL, cred: cred, conn: &conn{}}
	_, err := probe.listTools(ctx)
	switch {
	case errors.Is(err, errUnauthorized) && cred.value == "":
		return status.Error(codes.FailedPrecondition, "the MCP server asks for a sign-in or a key")
	case errors.Is(err, errUnauthorized):
		return status.Error(codes.InvalidArgument, "the MCP server refused the key")
	case err != nil:
		return status.Errorf(codes.FailedPrecondition, "list the tools of %s: %v", mcpURL, err)
	}
	g := &grant{user: user, connector: connector, kind: kind, mcpURL: mcpURL}
	sec := secrets{APIKey: cred.value, Header: cred.header}
	if err := s.store.putGrant(ctx, g, func(id string) ([]byte, error) {
		return s.seal.seal(ctx, user, sec, grantData(id, user, connector))
	}); err != nil {
		return err
	}
	s.dropConn(g.id)
	s.changed(ctx, g)
	log.WithFunc("connect.Service.connectDirect").Infof(ctx, "user %s connected %s without a sign-in, with a key: %t (grant %s)", user, connector, cred.value != "", g.id)
	return nil
}

func answeredBy(fs flowSecrets, iss string) bool {
	if fs.Issuer == "" || iss == "" {
		return fs.Issuer == "" || !fs.IssRequired
	}
	return strings.TrimSuffix(iss, "/") == strings.TrimSuffix(fs.Issuer, "/")
}
