package oauth

import (
	"context"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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

// Consent is what the consent page shows of a request it may approve; Existing marks a client that already holds a live grant of the user,
// which an approval renews, and Account a first-party client that signs in to the whole account.
type Consent struct {
	ClientName string `json:"client_name"`
	ClientID   string `json:"client_id"`
	// ClientHost is the verified host of the client's metadata document, empty for a self-registered client.
	ClientHost   string `json:"client_host"`
	RedirectHost string `json:"redirect_host"`
	Loopback     bool   `json:"loopback"`
	Existing     bool   `json:"existing"`
	Account      bool   `json:"account"`
	DeviceName   string `json:"device_name"`
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

// Check validates req of user and returns what the consent page shows.
func (s *Service) Check(ctx context.Context, user string, req Request) (Consent, error) {
	c, err := s.check(ctx, req)
	if err != nil {
		return Consent{}, err
	}
	existing := ""
	if !c.firstParty {
		if existing, err = s.store.liveGrant(ctx, user, c.id); err != nil {
			return Consent{}, err
		}
	}
	host, loopback := redirectHost(req.RedirectURI)
	return Consent{
		ClientName: c.name, ClientID: c.id, ClientHost: c.host, RedirectHost: host, Loopback: loopback, Existing: existing != "",
		Account: c.firstParty, DeviceName: req.DeviceName, Defaults: Caps{MaxBoxes: DefaultMaxBoxes, MaxRunning: DefaultMaxRunning, Days: DefaultDays},
	}, nil
}

// Approve records user's consent to req within caps, which a first-party client goes without, and returns where the browser takes the code.
func (s *Service) Approve(ctx context.Context, user string, req Request, caps Caps) (string, error) {
	c, err := s.check(ctx, req)
	if err != nil {
		return "", err
	}
	if c.firstParty {
		caps = Caps{}
	} else if err := caps.check(); err != nil {
		return "", err
	}
	value := randomHex(secretBytes)
	now := time.Now()
	if err := s.store.addCode(ctx, code{
		user: user, client: c, redirectURI: req.RedirectURI, challenge: req.CodeChallenge, caps: caps, label: req.DeviceName,
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
		return client{}, &AuthorizeError{Code: invalidRequest, Description: "redirect_uri is not one the client registered"}
	}
	var refusal *AuthorizeError
	switch {
	case req.ResponseType != codeParam:
		refusal = &AuthorizeError{Code: "unsupported_response_type", Description: "response_type must be code"}
	case req.ChallengeMethod != "S256" || len(req.CodeChallenge) != challengeLen:
		refusal = &AuthorizeError{Code: invalidRequest, Description: "PKCE with code_challenge_method S256 is required"}
	case c.firstParty && req.Scope != AccountScope:
		refusal = &AuthorizeError{Code: "invalid_scope", Description: "the scope of " + c.id + " is " + AccountScope}
	case !c.firstParty && !scopeAllowed(req.Scope):
		refusal = &AuthorizeError{Code: "invalid_scope", Description: "the only scope is " + Scope}
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

func scopeAllowed(scope string) bool {
	for s := range strings.FieldsSeq(scope) {
		if s != Scope {
			return false
		}
	}
	return true
}
