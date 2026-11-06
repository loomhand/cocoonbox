package oauth

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/loomhand/cocoonbox/api/utils"
)

const challengeLen = 43

// Request is one authorization request as the client sent it to AuthorizePath.
type Request struct {
	ResponseType    string
	ClientID        string
	RedirectURI     string
	State           string
	CodeChallenge   string
	ChallengeMethod string
	Scope           string
	Resource        string
	// DeviceName names the device a first-party client signs in, as the consent page shows it.
	DeviceName string
}

// ParseRequest reads an authorization request from its query.
func ParseRequest(q url.Values) Request {
	return Request{
		ResponseType:    q.Get("response_type"),
		ClientID:        q.Get("client_id"),
		RedirectURI:     q.Get("redirect_uri"),
		State:           q.Get("state"),
		CodeChallenge:   q.Get("code_challenge"),
		ChallengeMethod: q.Get("code_challenge_method"),
		Scope:           q.Get(scopeParam),
		Resource:        q.Get("resource"),
		DeviceName:      strings.TrimSpace(q.Get("device_name")),
	}
}

// Consent is what the consent page shows of a request: Existing marks a live grant the approval renews, Chosen its boxes, Connectors its Connect grants, Connect a request for ConnectScope, Account a whole-account first-party client.
type Consent struct {
	ClientName string `json:"client_name"`
	ClientID   string `json:"client_id"`
	// ClientHost is the verified host of the client's metadata document, empty for a self-registered client.
	ClientHost   string   `json:"client_host"`
	RedirectHost string   `json:"redirect_host"`
	Loopback     bool     `json:"loopback"`
	Existing     bool     `json:"existing"`
	Chosen       []string `json:"chosen"`
	Connectors   []string `json:"-"`
	Connect      bool     `json:"connect"`
	Account      bool     `json:"account"`
	DeviceName   string   `json:"device_name"`
	Defaults     Caps     `json:"defaults"`
}

// AuthorizeError refuses an authorization request; an empty Redirect means the client or its redirect URI is not trusted, so the page shows the error.
type AuthorizeError struct {
	Code        string
	Description string
	Redirect    string
}

func (e *AuthorizeError) Error() string { return e.Description }

// Check validates req of user and returns what the consent page shows.
func (s *Service) Check(ctx context.Context, user string, req Request) (Consent, error) {
	c, err := s.check(ctx, req)
	if err != nil {
		return Consent{}, err
	}
	var existing string
	var chosen, connectors []string
	if !c.firstParty {
		if existing, err = s.store.liveGrant(ctx, user, c.id); err != nil {
			return Consent{}, err
		}
	}
	if existing != "" {
		if chosen, err = s.store.chosen(ctx, existing); err != nil {
			return Consent{}, err
		}
		if connectors, err = s.store.chosenConnectors(ctx, existing); err != nil {
			return Consent{}, err
		}
	}
	host, loopback := redirectHost(req.RedirectURI)
	connect, _ := agentScope(req.Scope)
	return Consent{
		ClientName: c.name, ClientID: c.id, ClientHost: c.host, RedirectHost: host, Loopback: loopback, Existing: existing != "", Chosen: chosen,
		Connectors: connectors, Connect: connect && !c.firstParty, Account: c.firstParty, DeviceName: req.DeviceName,
		Defaults: Caps{MaxBoxes: defaultMaxBoxes, MaxRunning: defaultMaxRunning, Days: defaultDays},
	}, nil
}

// Approve records user's consent to req within caps, the user's own boxes of chosen and the Connect grants connectors (none for a first-party client), and returns where the browser takes the code.
func (s *Service) Approve(ctx context.Context, user string, req Request, caps Caps, chosen, connectors []string) (string, error) {
	c, err := s.check(ctx, req)
	if err != nil {
		return "", err
	}
	if c.firstParty {
		caps, chosen, connectors = Caps{}, nil, nil
	} else if err := caps.check(); err != nil {
		return "", err
	}
	if connect, _ := agentScope(req.Scope); len(connectors) != 0 && !connect {
		return "", fmt.Errorf("%w: connected accounts need the %s scope", ErrInvalid, ConnectScope)
	}
	value := utils.RandomHex(secretBytes)
	now := time.Now()
	if err := s.store.addCode(ctx, code{
		user: user, client: c, redirectURI: req.RedirectURI, challenge: req.CodeChallenge, caps: caps, chosen: chosen, connectors: connectors, label: req.DeviceName,
		grantExpires: now.AddDate(0, 0, caps.Days), expiresAt: now.Add(codeTTL), codeSHA256: utils.HashHex(value),
	}); err != nil {
		return "", err
	}
	return s.redirect(req, url.Values{codeParam: {value}}), nil
}

// Deny returns where the browser takes the user's refusal of req.
func (s *Service) Deny(ctx context.Context, req Request) (string, error) {
	if _, err := s.check(ctx, req); err != nil {
		return "", err
	}
	return s.redirect(req, url.Values{errorParam: {"access_denied"}, errorDescriptionParam: {"the user denied access"}}), nil
}

// check resolves req's client and checks req against it; past the redirect URI check, a refusal goes back to the client.
func (s *Service) check(ctx context.Context, req Request) (client, error) {
	c, err := s.client(ctx, req.ClientID)
	if err != nil {
		return client{}, &AuthorizeError{Code: "invalid_client", Description: err.Error()}
	}
	if req.RedirectURI == "" || !c.allows(req.RedirectURI) {
		return client{}, &AuthorizeError{Code: invalidRequest, Description: "redirect_uri is not one the client registered"}
	}
	_, scopeOK := agentScope(req.Scope)
	var refusal *AuthorizeError
	switch {
	case req.ResponseType != codeParam:
		refusal = &AuthorizeError{Code: "unsupported_response_type", Description: "response_type must be code"}
	case req.ChallengeMethod != "S256" || len(req.CodeChallenge) != challengeLen:
		refusal = &AuthorizeError{Code: invalidRequest, Description: "PKCE with code_challenge_method S256 is required"}
	case c.firstParty && req.Scope != AccountScope:
		refusal = &AuthorizeError{Code: "invalid_scope", Description: "the scope of " + c.id + " is " + AccountScope}
	case !c.firstParty && !scopeOK:
		refusal = &AuthorizeError{Code: "invalid_scope", Description: "the scope is " + Scope + ", or " + agentScopes}
	case len(req.DeviceName) > maxDeviceName || !utf8.ValidString(req.DeviceName):
		refusal = &AuthorizeError{Code: invalidRequest, Description: "device_name is at most " + strconv.Itoa(maxDeviceName) + " bytes of UTF-8"}
	case req.Resource != "" && !s.covers(req.Resource):
		refusal = &AuthorizeError{Code: "invalid_target", Description: "resource must be " + s.resource}
	default:
		return c, nil
	}
	refusal.Redirect = s.redirect(req, url.Values{errorParam: {refusal.Code}, errorDescriptionParam: {refusal.Description}})
	return client{}, refusal
}

// redirect adds params, the state and the issuer (RFC 9207) to req's redirect URI.
func (s *Service) redirect(req Request, params url.Values) string {
	u, _ := url.Parse(req.RedirectURI)
	q := u.Query()
	maps.Copy(q, params)
	if req.State != "" {
		q.Set("state", req.State)
	}
	q.Set("iss", s.issuer)
	u.RawQuery = q.Encode()
	return u.String()
}

// covers reports whether uri is the resource or an MCP endpoint under it, with the scheme and host in any case.
func (s *Service) covers(uri string) bool {
	u, err := url.Parse(uri)
	base, _ := url.Parse(s.resource)
	if err != nil || !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	path := strings.TrimSuffix(u.Path, "/")
	return path == base.Path || strings.HasPrefix(path, base.Path+"/")
}

// agentScope reports whether an agent's scope asks for ConnectScope, and whether it is Scope with ConnectScope or without; an absent scope is Scope.
func agentScope(scope string) (connect, ok bool) {
	fields := strings.Fields(scope)
	for _, f := range fields {
		if f != Scope && f != ConnectScope {
			return false, false
		}
	}
	return slices.Contains(fields, ConnectScope), len(fields) == 0 || slices.Contains(fields, Scope)
}

// grantedScope is the scope a token of a grant reports: ConnectScope too once the user ticked an account for it.
func grantedScope(connectors bool) string {
	if connectors {
		return agentScopes
	}
	return Scope
}
