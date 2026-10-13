package oauth

import (
	"context"
	"maps"
	"net/url"
	"strings"
	"time"
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
		Scope:           q.Get("scope"),
		Resource:        q.Get("resource"),
	}
}

// Consent is what the consent page shows of a request it may approve.
type Consent struct {
	ClientName string `json:"client_name"`
	ClientID   string `json:"client_id"`
	// ClientHost is the verified host of the client's metadata document, empty for a self-registered client.
	ClientHost   string `json:"client_host"`
	RedirectHost string `json:"redirect_host"`
	Loopback     bool   `json:"loopback"`
	Defaults     Caps   `json:"defaults"`
}

// AuthorizeError refuses an authorization request; Redirect takes the error back to the client, and is empty when the
// client or its redirect URI is not trusted, so the page shows the error instead.
type AuthorizeError struct {
	Code        string
	Description string
	Redirect    string
}

func (e *AuthorizeError) Error() string { return e.Description }

// Check validates req and returns what the consent page shows.
func (s *Service) Check(ctx context.Context, req Request) (Consent, error) {
	c, err := s.check(ctx, req)
	if err != nil {
		return Consent{}, err
	}
	host, loopback := redirectHost(req.RedirectURI)
	return Consent{
		ClientName: c.name, ClientID: c.id, ClientHost: c.host, RedirectHost: host, Loopback: loopback,
		Defaults: Caps{MaxBoxes: DefaultMaxBoxes, MaxRunning: DefaultMaxRunning, Days: DefaultDays},
	}, nil
}

// Approve records user's consent to req within caps and returns where the browser takes the code.
func (s *Service) Approve(ctx context.Context, user string, req Request, caps Caps) (string, error) {
	c, err := s.check(ctx, req)
	if err != nil {
		return "", err
	}
	if err := caps.check(); err != nil {
		return "", err
	}
	value := randomHex(secretBytes)
	now := time.Now()
	if err := s.store.addCode(ctx, code{
		user: user, client: c, redirectURI: req.RedirectURI, challenge: req.CodeChallenge, caps: caps,
		grantExpires: now.AddDate(0, 0, caps.Days), expiresAt: now.Add(codeTTL), codeSHA256: hashHex(value),
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
		return client{}, &AuthorizeError{Code: "invalid_request", Description: "redirect_uri is not one the client registered"}
	}
	var refusal *AuthorizeError
	switch {
	case req.ResponseType != codeParam:
		refusal = &AuthorizeError{Code: "unsupported_response_type", Description: "response_type must be code"}
	case req.ChallengeMethod != "S256" || len(req.CodeChallenge) != challengeLen:
		refusal = &AuthorizeError{Code: "invalid_request", Description: "PKCE with code_challenge_method S256 is required"}
	case !scopeAllowed(req.Scope):
		refusal = &AuthorizeError{Code: "invalid_scope", Description: "the only scope is " + Scope}
	case req.Resource != "" && !sameResource(req.Resource, s.resource):
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

func scopeAllowed(scope string) bool {
	for s := range strings.FieldsSeq(scope) {
		if s != Scope {
			return false
		}
	}
	return true
}

// sameResource compares resource URLs with the scheme and host in any case and without a trailing slash.
func sameResource(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimSuffix(ua.Path, "/") == strings.TrimSuffix(ub.Path, "/") && ua.RawQuery == "" && ua.Fragment == ""
}
