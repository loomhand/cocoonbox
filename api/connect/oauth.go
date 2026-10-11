package connect

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

const (
	authNone        = "none"
	authSecretBasic = "client_secret_basic"
	maxMetadataSize = 1 << 20
	tokenBytes      = 32
)

var challengeParam = regexp.MustCompile(`([a-zA-Z_]+)="([^"]*)"`)

type client struct {
	ID         string `json:"id"`
	Secret     string `json:"secret,omitzero"`
	AuthMethod string `json:"auth_method,omitzero"`
	TokenURL   string `json:"token_url,omitzero"`
	RevokeURL  string `json:"revoke_url,omitzero"`
	// Resource is the MCP server's resource indicator (RFC 8707), sent on every token request.
	Resource string `json:"resource,omitzero"`
}

type tokens struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type oauthError struct {
	status      int
	code        string
	description string
}

func (e *oauthError) Error() string {
	return strings.TrimSuffix(fmt.Sprintf("token endpoint answered %d %s: %s", e.status, e.code, e.description), ": ")
}

type authorization struct {
	authorizeURL string
	client       client
	scopes       []string
}

type resourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

type serverMetadata struct {
	AuthorizationEndpoint    string   `json:"authorization_endpoint"`
	TokenEndpoint            string   `json:"token_endpoint"`
	RegistrationEndpoint     string   `json:"registration_endpoint"`
	RevocationEndpoint       string   `json:"revocation_endpoint"`
	CodeChallengeMethods     []string `json:"code_challenge_methods_supported"`
	ClientIDMetadataDocument bool     `json:"client_id_metadata_document_supported"`
}

// discover follows MCP authorization (2025-11-25); its client is preset, else a metadata document, else registered.
func (s *Service) discover(ctx context.Context, mcpURL string, preset client) (*authorization, error) {
	target, err := url.Parse(mcpURL)
	if err != nil {
		return nil, err
	}
	metadataURL, scope, err := s.challenge(ctx, mcpURL)
	if err != nil {
		return nil, err
	}
	candidates := []string{wellKnown(target, "oauth-protected-resource", true), wellKnown(target, "oauth-protected-resource", false)}
	if metadataURL != "" {
		candidates = []string{metadataURL}
	}
	var prm resourceMetadata
	if err = s.firstMetadata(ctx, candidates, &prm); err != nil && metadataURL != "" {
		return nil, fmt.Errorf("protected resource metadata of %s: %w", mcpURL, err)
	}
	if err != nil {
		// A server of MCP authorization 2025-03-26 has no resource metadata; its origin is the authorization server.
		prm.AuthorizationServers = []string{target.Scheme + "://" + target.Host}
	}
	if len(prm.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("%s names no authorization server", mcpURL)
	}
	resource := cmp.Or(prm.Resource, mcpURL)
	if r, parseErr := url.Parse(resource); parseErr != nil || r.Scheme != target.Scheme || r.Host != target.Host {
		return nil, fmt.Errorf("%s names resource %q on another origin", mcpURL, resource)
	}
	issuer, err := url.Parse(prm.AuthorizationServers[0])
	if err != nil {
		return nil, fmt.Errorf("authorization server %q: %w", prm.AuthorizationServers[0], err)
	}
	var as serverMetadata
	if err = s.firstMetadata(ctx, []string{
		wellKnown(issuer, "oauth-authorization-server", true),
		wellKnown(issuer, "openid-configuration", true),
		strings.TrimSuffix(issuer.String(), "/") + "/.well-known/openid-configuration",
	}, &as); err != nil {
		return nil, fmt.Errorf("authorization server metadata of %s: %w", issuer, err)
	}
	switch {
	case as.AuthorizationEndpoint == "" || as.TokenEndpoint == "":
		return nil, fmt.Errorf("authorization server %s names no authorization or token endpoint", issuer)
	case !slices.Contains(as.CodeChallengeMethods, "S256"):
		return nil, fmt.Errorf("authorization server %s does not offer PKCE S256", issuer)
	}
	auth := &authorization{authorizeURL: as.AuthorizationEndpoint, scopes: prm.ScopesSupported}
	if scope != "" {
		auth.scopes = strings.Fields(scope)
	}
	switch {
	case preset.ID != "":
		auth.client = preset
	case as.ClientIDMetadataDocument:
		auth.client = client{ID: s.publicURL + ClientMetadataPath, AuthMethod: authNone}
	case as.RegistrationEndpoint != "":
		if auth.client, err = s.register(ctx, as.RegistrationEndpoint); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("authorization server %s takes neither a client metadata document nor dynamic registration", issuer)
	}
	auth.client.TokenURL, auth.client.RevokeURL, auth.client.Resource = as.TokenEndpoint, as.RevocationEndpoint, resource
	return auth, nil
}

// challenge sends the MCP server an unauthenticated initialize and reads its WWW-Authenticate answer.
func (s *Service) challenge(ctx context.Context, mcpURL string) (string, string, error) {
	body, _ := json.Marshal(rpcRequest{JSONRPC: jsonRPCVersion, ID: 1, Method: methodInitialize, Params: initializeParams()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("reach %s: %w", mcpURL, err)
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode/100 == 2:
		return "", "", errNoSignIn
	case resp.StatusCode != http.StatusUnauthorized:
		return "", "", fmt.Errorf("%s answered %d without a sign-in; connect it with a key instead", mcpURL, resp.StatusCode)
	}
	params := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(resp.Header.Get("WWW-Authenticate"), -1) {
		params[m[1]] = m[2]
	}
	return params["resource_metadata"], params["scope"], nil
}

func (s *Service) firstMetadata(ctx context.Context, urls []string, v any) error {
	var errs []error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		status, body, err := s.send(req)
		if err == nil && status == http.StatusOK {
			return json.Unmarshal(body, v)
		}
		errs = append(errs, cmp.Or(err, fmt.Errorf("%s answered %d", u, status)))
	}
	return errors.Join(errs...)
}

// register registers a public client with PKCE by dynamic client registration (RFC 7591).
func (s *Service) register(ctx context.Context, endpoint string) (client, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "CocoonBox",
		"redirect_uris":              []string{s.publicURL + CallbackPath},
		"grant_types":                []string{grantAuthorizationCode, grantRefreshToken},
		"response_types":             []string{responseCode},
		"token_endpoint_auth_method": authNone,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return client{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	status, answer, err := s.send(req)
	if err != nil {
		return client{}, fmt.Errorf("register at %s: %w", endpoint, err)
	}
	var reg struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		AuthMethod   string `json:"token_endpoint_auth_method"`
	}
	if status/100 != 2 || json.Unmarshal(answer, &reg) != nil || reg.ClientID == "" {
		return client{}, fmt.Errorf("register at %s: answered %d", endpoint, status)
	}
	return client{ID: reg.ClientID, Secret: reg.ClientSecret, AuthMethod: cmp.Or(reg.AuthMethod, authNone)}, nil
}

func (s *Service) token(ctx context.Context, c client, form url.Values) (*tokens, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	if c.AuthMethod != authSecretBasic {
		form.Set("client_id", c.ID)
		if c.Secret != "" {
			form.Set("client_secret", c.Secret)
		}
	}
	if c.Resource != "" {
		form.Set("resource", c.Resource)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.AuthMethod == authSecretBasic {
		req.SetBasicAuth(url.QueryEscape(c.ID), url.QueryEscape(c.Secret))
	}
	status, body, err := s.send(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	var t tokens
	decodeErr := json.Unmarshal(body, &t)
	if status/100 != 2 || t.Error != "" || decodeErr != nil || t.AccessToken == "" {
		return nil, &oauthError{status: status, code: cmp.Or(t.Error, "invalid_response"), description: t.ErrorDescription}
	}
	return &t, nil
}

// revoke ends the grant at the vendor; false when the vendor offers no revoke call.
func (s *Service) revoke(ctx context.Context, kind revokeKind, c client, sec secrets) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	var req *http.Request
	var err error
	switch {
	case kind == revokeGitHub && sec.AccessToken != "":
		body, _ := json.Marshal(map[string]string{"access_token": sec.AccessToken})
		if req, err = http.NewRequestWithContext(ctx, http.MethodDelete, c.RevokeURL, bytes.NewReader(body)); err != nil {
			return false, err
		}
		req.SetBasicAuth(c.ID, c.Secret)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Content-Type", "application/json")
	case kind == revokeRFC7009 && c.RevokeURL != "" && cmp.Or(sec.RefreshToken, sec.AccessToken) != "":
		form := url.Values{"token": {cmp.Or(sec.RefreshToken, sec.AccessToken)}}
		if sec.RefreshToken != "" {
			form.Set("token_type_hint", grantRefreshToken)
		}
		if c.AuthMethod != authSecretBasic {
			form.Set("client_id", c.ID)
			if c.Secret != "" {
				form.Set("client_secret", c.Secret)
			}
		}
		if req, err = http.NewRequestWithContext(ctx, http.MethodPost, c.RevokeURL, strings.NewReader(form.Encode())); err != nil {
			return false, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c.AuthMethod == authSecretBasic {
			req.SetBasicAuth(url.QueryEscape(c.ID), url.QueryEscape(c.Secret))
		}
	default:
		return false, nil
	}
	status, _, err := s.send(req)
	if err != nil {
		return false, fmt.Errorf("revoke: %w", err)
	}
	if status/100 != 2 {
		return false, fmt.Errorf("revoke: the vendor answered %d", status)
	}
	return true, nil
}

// accountLabel reads the account's name from the vendor's user endpoint, with the endpoint's HTTP status.
func (s *Service) accountLabel(ctx context.Context, e *entry, accessToken string) (string, int) {
	if e.userInfoURL == "" {
		return "", 0
	}
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.userInfoURL, nil)
	if err != nil {
		return "", 0
	}
	req.Header.Set(headerAuthorization, "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	status, body, err := s.send(req)
	if err != nil || status != http.StatusOK {
		return "", status
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(body, &fields) != nil {
		return "", status
	}
	var label string
	_ = json.Unmarshal(fields[e.userInfoName], &label)
	return label, status
}

func (s *Service) send(req *http.Request) (int, []byte, error) {
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataSize))
	return resp.StatusCode, body, err
}

func pkce() (string, string) {
	verifier := randomToken()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomToken() string {
	raw := make([]byte, tokenBytes)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// wellKnown is u's /.well-known/<name> URL, with u's path appended when withPath is set (RFC 8414, RFC 9728).
func wellKnown(u *url.URL, name string, withPath bool) string {
	out := u.Scheme + "://" + u.Host + "/.well-known/" + name
	if path := strings.TrimSuffix(u.EscapedPath(), "/"); withPath && path != "" {
		out += path
	}
	return out
}
