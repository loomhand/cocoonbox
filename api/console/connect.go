package console

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/connect"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

const (
	// connectCookie carries a flow's nonce to the callback, which the Strict, /cb/-scoped session cookie never reaches.
	connectCookie = "cb_connect"
	connectAge    = 10 * time.Minute
)

type connectTool struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	ReadOnly    bool   `json:"read_only"`
	Destructive bool   `json:"destructive"`
	Policy      string `json:"policy"`
	Effective   string `json:"effective"`
}

type connector struct {
	ID          string        `json:"id"`
	Kind        string        `json:"kind"`
	Name        string        `json:"name"`
	Account     string        `json:"account"`
	Status      string        `json:"status"`
	Scopes      []string      `json:"scopes"`
	MCPURL      string        `json:"mcp_url"`
	ConnectedAt int64         `json:"connected_at_ms"`
	Tools       []connectTool `json:"tools"`
	ToolsError  string        `json:"tools_error,omitempty"`
}

type catalogEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Available   bool   `json:"available"`
	InstallURL  string `json:"install_url,omitempty"`
	GrantOnly   bool   `json:"grant_only"`
	Checkable   bool   `json:"checkable"`
}

// EnableConnect serves the Connect page and its calls over conn, and the OAuth callback through ConnectCallback.
func (s *Server) EnableConnect(conn *connect.Service) {
	s.connect = conn
	s.mux.HandleFunc("GET "+connect.ConsolePath, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "static/connect.html")
	})
	s.mux.HandleFunc("GET "+Prefix+"api/connect", s.signedIn(s.connectState))
	s.mux.HandleFunc("POST "+Prefix+"api/connect/{connector}/begin", s.signedIn(s.beginConnect))
	s.mux.HandleFunc("PUT "+Prefix+"api/connect/{connector}/policy", s.signedIn(s.setConnectPolicy))
	s.mux.HandleFunc("DELETE "+Prefix+"api/connect/{connector}", s.signedIn(s.disconnect))
	s.mux.HandleFunc("POST "+Prefix+"api/connect/{connector}/check", s.signedIn(s.checkConnector))
	s.mux.HandleFunc("GET "+Prefix+"api/connect/activity", s.signedIn(s.connectActivity))
	s.mux.HandleFunc("POST "+Prefix+"api/connect/approvals/{id}/answer", s.signedIn(s.approve))
}

// ConnectCallback finishes a sign-in only in the browser that began it, while its console session is live.
func (s *Server) ConnectCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.SetCookie(w, &http.Cookie{Name: connectCookie, Path: connect.Prefix, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	logger := log.WithFunc("console.Server.ConnectCallback")
	if q.Get("state") == "" && q.Get("setup_action") != "" {
		redirect(w, r, "installed", "github")
		return
	}
	c, err := r.Cookie(connectCookie)
	if err != nil {
		logger.Warnf(r.Context(), "callback without the connect cookie from %s", clientAddr(r))
		redirect(w, r, "error", "This sign-in did not start in this browser. Start it again from this page.")
		return
	}
	if vendorErr := q.Get("error"); vendorErr != "" {
		redirect(w, r, "error", strings.TrimSuffix("The vendor answered "+vendorErr+": "+q.Get("error_description"), ": "))
		return
	}
	id, err := s.connect.Complete(r.Context(), c.Value, q.Get("state"), q.Get("code"))
	if err != nil {
		logger.Warnf(r.Context(), "complete a sign-in from %s: %v", clientAddr(r), err)
		message := "Connecting failed; try again."
		if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown && st.Code() != codes.Internal {
			message = st.Message()
		}
		redirect(w, r, "error", message)
		return
	}
	redirect(w, r, "connected", id)
}

func (s *Server) connectState(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.connect.Catalog(r.Context(), &controlv1.CatalogRequest{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list, err := s.connect.List(s.ownerCtx(r), &controlv1.ListConnectorsRequest{WithTools: true})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pending, err := s.connect.Pending(r.Context(), caller(r).ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp := struct {
		Catalog    []catalogEntry            `json:"catalog"`
		Connectors []connector               `json:"connectors"`
		Pending    []connect.PendingApproval `json:"pending"`
	}{Catalog: []catalogEntry{}, Connectors: []connector{}, Pending: pending}
	for _, e := range catalog.GetEntries() {
		resp.Catalog = append(resp.Catalog, catalogEntry{
			ID: e.GetId(), Name: e.GetName(), Description: e.GetDescription(), Available: e.GetAvailable(), InstallURL: e.GetInstallUrl(),
			GrantOnly: e.GetGrantOnly(), Checkable: e.GetCheckable(),
		})
	}
	for _, c := range list.GetConnectors() {
		resp.Connectors = append(resp.Connectors, toConnector(c))
	}
	writeJSON(w, resp)
}

func (s *Server) beginConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		URL    string `json:"url"`
		Header string `json:"header"`
		Key    string `json:"key"`
	}
	if !decode(w, r, &req) {
		return
	}
	session, _ := r.Cookie(cookieName)
	started, err := s.connect.Start(r.Context(), connect.StartRequest{
		User: caller(r).ID, SessionSHA256: sessionSHA256(session.Value), Connector: r.PathValue("connector"),
		Name: req.Name, URL: req.URL, Header: req.Header, Key: req.Key,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if started.URL == "" {
		writeJSON(w, map[string]string{"connected": started.Connector})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: connectCookie, Value: started.Nonce, Path: connect.Prefix, MaxAge: int(connectAge.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, map[string]string{"url": started.URL})
}

func (s *Server) setConnectPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool   string `json:"tool"`
		Policy string `json:"policy"`
	}
	if !decode(w, r, &req) {
		return
	}
	policy, ok := controlv1.Policy_value["POLICY_"+strings.ToUpper(req.Policy)]
	if req.Policy == "default" {
		policy, ok = int32(controlv1.Policy_POLICY_UNSPECIFIED), true
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "a policy is allow, ask, off or default")
		return
	}
	c, err := s.connect.SetPolicy(s.ownerCtx(r), &controlv1.SetPolicyRequest{ConnectorId: r.PathValue("connector"), Tool: req.Tool, Policy: controlv1.Policy(policy)})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, toConnector(c))
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) {
	revoked, err := s.connect.Remove(r.Context(), caller(r).ID, r.PathValue("connector"), connect.DoorConsole)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]bool{"revoked": revoked})
}

func (s *Server) checkConnector(w http.ResponseWriter, r *http.Request) {
	account, err := s.connect.Check(r.Context(), caller(r).ID, r.PathValue("connector"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]string{"account": account})
}

func (s *Server) connectActivity(w http.ResponseWriter, r *http.Request) {
	list, err := s.connect.Activity(s.ownerCtx(r), &controlv1.ActivityRequest{ConnectorId: r.URL.Query().Get("connector")})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type entry struct {
		At        int64  `json:"at_ms"`
		Connector string `json:"connector"`
		Tool      string `json:"tool"`
		Door      string `json:"door"`
		Caller    string `json:"caller"`
		Decision  string `json:"decision"`
		Outcome   string `json:"outcome"`
		Status    int32  `json:"upstream_status"`
		LatencyMs int64  `json:"latency_ms"`
	}
	out := make([]entry, 0, len(list.GetEntries()))
	for _, e := range list.GetEntries() {
		out = append(out, entry{
			At: e.GetAtMs(), Connector: e.GetConnectorId(), Tool: e.GetTool(), Door: e.GetDoor(), Caller: e.GetCaller(),
			Decision: e.GetDecision(), Outcome: e.GetOutcome(), Status: e.GetUpstreamStatus(), LatencyMs: e.GetLatencyMs(),
		})
	}
	writeJSON(w, out)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Decision string `json:"decision"`
	}
	if !decode(w, r, &req) {
		return
	}
	decision, ok := controlv1.ApprovalDecision_value["APPROVAL_DECISION_"+strings.ToUpper(req.Decision)]
	if !ok || decision == 0 {
		writeError(w, http.StatusBadRequest, "a decision is once, always or deny")
		return
	}
	if _, err := s.connect.Approve(s.ownerCtx(r), &controlv1.ApproveRequest{ApprovalId: r.PathValue("id"), Decision: controlv1.ApprovalDecision(decision)}); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toConnector(c *controlv1.Connector) connector {
	out := connector{
		ID: c.GetId(), Kind: c.GetKind(), Name: c.GetName(), Account: c.GetAccount(), Status: c.GetStatus(), Scopes: c.GetScopes(),
		MCPURL: c.GetMcpUrl(), ConnectedAt: c.GetConnectedAtMs(), Tools: []connectTool{}, ToolsError: c.GetToolsError(),
	}
	for _, t := range c.GetTools() {
		out.Tools = append(out.Tools, connectTool{
			Name: t.GetName(), Title: t.GetTitle(), Description: t.GetDescription(), ReadOnly: t.GetReadOnly(), Destructive: t.GetDestructive(),
			Policy: policyName(t.GetPolicy()), Effective: policyName(t.GetEffective()),
		})
	}
	return out
}

func policyName(p controlv1.Policy) string {
	if p == controlv1.Policy_POLICY_UNSPECIFIED {
		return "default"
	}
	return strings.ToLower(strings.TrimPrefix(p.String(), "POLICY_"))
}

func redirect(w http.ResponseWriter, r *http.Request, key, value string) {
	http.Redirect(w, r, connect.ConsolePath+"?"+url.Values{key: {value}}.Encode(), http.StatusSeeOther)
}

func sessionSHA256(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
