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

// Start returns the vendor's authorization URL, empty when a custom key connected at once, and the connector id.
func (s *Service) Start(ctx context.Context, req StartRequest) (string, string, error) {
	e := s.entry(req.Connector)
	switch {
	case e == nil:
		return "", "", status.Errorf(codes.InvalidArgument, "unknown connector %q", req.Connector)
	case !e.available():
		return "", "", status.Errorf(codes.FailedPrecondition, "%s is not set up on this server", e.name)
	}
	connector, mcpURL := e.id, e.mcpURL
	if e.auth == authCustom {
		var err error
		if connector, mcpURL, err = s.customTarget(req); err != nil {
			return "", "", err
		}
		if req.Key != "" {
			return "", connector, s.connectKey(ctx, req.User, connector, mcpURL, cmp.Or(req.Header, "Authorization"), req.Key)
		}
	}
	verifier, challenge := pkce()
	state := randomToken(32)
	fs := flowSecrets{Verifier: verifier}
	q := url.Values{
		"response_type":         {responseCode},
		"redirect_uri":          {s.publicURL + CallbackPath},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	authorizeURL := e.authorizeURL
	if e.auth == authOAuth {
		fs.Scopes = e.scopes
		q.Set("client_id", e.client.ID)
		maps.Copy(q, e.params)
	} else {
		discoverCtx, cancel := context.WithTimeout(ctx, setupTimeout)
		auth, err := s.discover(discoverCtx, mcpURL)
		cancel()
		if err != nil {
			return "", "", status.Errorf(codes.FailedPrecondition, "sign-in to %s: %v", mcpURL, err)
		}
		authorizeURL, fs.Client, fs.Scopes = auth.authorizeURL, auth.client, auth.scopes
		q.Set("client_id", auth.client.ID)
		q.Set("resource", auth.client.Resource)
	}
	if len(fs.Scopes) > 0 {
		q.Set("scope", strings.Join(fs.Scopes, " "))
	}
	target, err := url.Parse(authorizeURL)
	if err != nil {
		return "", "", status.Errorf(codes.FailedPrecondition, "authorization endpoint %q: %v", authorizeURL, err)
	}
	merged := target.Query()
	maps.Copy(merged, q)
	target.RawQuery = merged.Encode()
	f := &flow{
		id: "cf_" + randomHex(idBytes), user: req.User, connector: connector, kind: e.id, mcpURL: mcpURL,
		stateSHA256: hashHex(state), sessionSHA256: req.SessionSHA256, expiresAt: time.Now().Add(flowTTL),
	}
	if f.sealed, err = s.seal.seal(ctx, f.user, fs, flowData(f.id, f.user, f.connector)); err != nil {
		return "", "", err
	}
	if err := s.store.insertFlow(ctx, f); err != nil {
		return "", "", err
	}
	log.WithFunc("connect.Service.Start").Infof(ctx, "user %s began connecting %s (flow %s)", req.User, connector, f.id)
	return target.String(), connector, nil
}

// Complete exchanges code for the flow of state that user's console session began, and returns the connector id.
func (s *Service) Complete(ctx context.Context, user, sessionSHA256, state, code string) (string, error) {
	f, err := s.store.takeFlow(ctx, user, hashHex(state), sessionSHA256)
	if errors.Is(err, errNoFlow) {
		return "", status.Error(codes.PermissionDenied, err.Error())
	}
	if err != nil {
		return "", err
	}
	var fs flowSecrets
	if err = s.seal.open(ctx, user, f.sealed, flowData(f.id, f.user, f.connector), &fs); err != nil {
		return "", err
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
	g := &grant{
		user: user, connector: f.connector, kind: f.kind, mcpURL: f.mcpURL, account: s.accountLabel(ctx, e, t.AccessToken),
		scopes: fs.Scopes, expiresAt: expiry(t.ExpiresIn),
	}
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
	s.watchers.notify(user)
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
	if req.Key != "" && (!httpguts.ValidHeaderFieldName(cmp.Or(req.Header, "Authorization")) || !httpguts.ValidHeaderFieldValue(req.Key)) {
		return "", "", status.Error(codes.InvalidArgument, "the header name or the key is not valid in an HTTP header")
	}
	u.Fragment = ""
	return req.Name, u.String(), nil
}

// connectKey stores the key only after the server listed its tools with it.
func (s *Service) connectKey(ctx context.Context, user, connector, mcpURL, header, key string) error {
	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	probe := &upstream{s: s, url: mcpURL, cred: credential{header: header, value: key}, conn: &conn{}}
	if _, err := probe.listTools(ctx); errors.Is(err, errUnauthorized) {
		return status.Error(codes.InvalidArgument, "the MCP server refused the key")
	} else if err != nil {
		return status.Errorf(codes.FailedPrecondition, "list the tools of %s: %v", mcpURL, err)
	}
	g := &grant{user: user, connector: connector, kind: kindCustom, mcpURL: mcpURL}
	sec := secrets{APIKey: key, Header: header}
	if err := s.store.putGrant(ctx, g, func(id string) ([]byte, error) {
		return s.seal.seal(ctx, user, sec, grantData(id, user, connector))
	}); err != nil {
		return err
	}
	s.dropConn(g.id)
	s.watchers.notify(user)
	log.WithFunc("connect.Service.connectKey").Infof(ctx, "user %s connected %s with a key (grant %s)", user, connector, g.id)
	return nil
}
