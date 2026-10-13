// Package oauth makes the control MCP an OAuth 2.1 protected resource (MCP authorization 2025-11-25): it registers
// clients, issues a code once the user consents in the console, and issues tokens that reach only the boxes their grant creates.
package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/CMGS/cocoonbox/api/users"
)

const (
	// ServerMetadataPath serves the RFC 8414 metadata of the issuer, the public origin.
	ServerMetadataPath = "/.well-known/oauth-authorization-server"
	// AuthorizePath is the console page where the user signs in and consents.
	AuthorizePath = "/cb/oauth/authorize"
	// TokenPath takes form posts, so it lives outside the JSON-only console.
	TokenPath = "/cocoonbox.oauth/token" //nolint:gosec // a path, not a credential
	// RegisterPath takes RFC 7591 registrations.
	RegisterPath = "/cocoonbox.oauth/register"
	// RevokePath takes RFC 7009 revocations.
	RevokePath = "/cocoonbox.oauth/revoke"
	// Scope is the one scope: manage the boxes the grant creates.
	Scope = "boxes"
	// AccessPrefix starts every access token.
	AccessPrefix = "cba_"

	// DefaultMaxBoxes fills the consent form.
	DefaultMaxBoxes = 2
	// DefaultMaxRunning fills the consent form.
	DefaultMaxRunning = 1
	// DefaultDays fills the consent form.
	DefaultDays = 30

	resourceMetadataPrefix = "/.well-known/oauth-protected-resource"
	codeParam              = "code"
	errorParam             = "error"
	errorDescriptionParam  = "error_description"
	authorizationCode      = "authorization_code"
	refreshToken           = "refresh_token"
	authNone               = "none"
	schemeHTTP             = "http"
	refreshPrefix          = "cbr_"
	grantPrefix            = "oag_"
	bearerPrefix           = "bearer "
	secretBytes            = 32
	grantIDBytes           = 8
	accessTTL              = time.Hour
	codeTTL                = time.Minute
	usedEvery              = time.Minute
	maxBoxesLimit          = 16
)

var (
	ErrNotFound     = errors.New("grant not found")
	ErrInvalid      = errors.New("invalid request")
	ErrInvalidToken = errors.New("invalid token")

	errInvalidGrant = errors.New("invalid grant")

	grantDays  = []int{1, 7, 30, 90}
	grantTypes = []string{authorizationCode, refreshToken}
)

// Accounts returns a user, so a disabled or deleted user's tokens stop working.
type Accounts interface {
	Get(id string) (users.User, error)
}

// Options names the database, the public origin, the protected resource and the users.
type Options struct {
	DSN string
	// Issuer is the api's public https origin.
	Issuer string
	// Resource is the canonical URL that covers every MCP endpoint a token reaches: the control MCP and the box MCPs under it.
	Resource string
	Accounts Accounts
}

// Caps bound a grant: how many boxes it may create, how many of them may run at once, and for how many days it lasts.
type Caps struct {
	MaxBoxes   int `json:"max_boxes"`
	MaxRunning int `json:"max_running"`
	Days       int `json:"days"`
}

func (c Caps) check() error {
	switch {
	case c.MaxBoxes < 1 || c.MaxBoxes > maxBoxesLimit:
		return fmt.Errorf("%w: max boxes must be in 1..%d", ErrInvalid, maxBoxesLimit)
	case c.MaxRunning < 1 || c.MaxRunning > c.MaxBoxes:
		return fmt.Errorf("%w: max running must be in 1..max boxes", ErrInvalid)
	case !slices.Contains(grantDays, c.Days):
		return fmt.Errorf("%w: a grant lasts 1, 7, 30 or 90 days", ErrInvalid)
	}
	return nil
}

// Grant is one client's access for one user; Boxes are the boxes it created that still exist.
type Grant struct {
	ID         string `json:"id"`
	User       string `json:"-"`
	ClientID   string `json:"client_id"`
	ClientName string `json:"client_name"`
	// ClientHost is the host of the client's metadata document URL, empty for a self-registered client.
	ClientHost string     `json:"client_host"`
	MaxBoxes   int        `json:"max_boxes"`
	MaxRunning int        `json:"max_running"`
	Boxes      []string   `json:"boxes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UsedAt     *time.Time `json:"used_at"`
	Seen       bool       `json:"seen"`
}

// Service is the authorization server and the token check of the control MCP.
type Service struct {
	store     *store
	accounts  Accounts
	issuer    string
	resource  string
	key       []byte
	fetch     *http.Client
	documents *documents
}

// Open connects to the database and applies the migrations.
func Open(ctx context.Context, opts Options) (*Service, error) {
	st, err := openStore(ctx, opts.DSN)
	if err != nil {
		return nil, err
	}
	key, err := st.key(ctx)
	if err != nil {
		st.close()
		return nil, err
	}
	dialer := &net.Dialer{Timeout: fetchTimeout, Control: refusePrivate}
	fetch := &http.Client{
		Transport:     &http.Transport{DialContext: dialer.DialContext, ForceAttemptHTTP2: true, TLSHandshakeTimeout: fetchTimeout},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Service{
		store: st, accounts: opts.Accounts, issuer: strings.TrimSuffix(opts.Issuer, "/"), resource: opts.Resource, key: key,
		fetch: fetch, documents: &documents{entries: map[string]cachedDocument{}},
	}, nil
}

// Close releases the database pool.
func (s *Service) Close() { s.store.close() }

// ResourceMetadataPath is where the protected resource's RFC 9728 metadata lives: the well-known prefix and the resource's path;
// the same metadata also answers under it, for clients that probe the path of the endpoint they use.
func (s *Service) ResourceMetadataPath() string {
	u, _ := url.Parse(s.resource)
	return resourceMetadataPrefix + u.Path
}

// Challenge is the WWW-Authenticate value of a 401 from the protected resource; invalid adds invalid_token for a token it refused.
func (s *Service) Challenge(invalid bool) string {
	value := `Bearer resource_metadata="` + s.issuer + s.ResourceMetadataPath() + `", scope="` + Scope + `"`
	if invalid {
		value += `, error="invalid_token"`
	}
	return value
}

// ServeResourceMetadata answers the protected resource metadata.
func (s *Service) ServeResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.resource,
		"authorization_servers":    []string{s.issuer},
		"scopes_supported":         []string{Scope},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "CocoonBox",
	})
}

// ServeServerMetadata answers the authorization server metadata.
func (s *Service) ServeServerMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         s.issuer,
		"authorization_endpoint":                         s.issuer + AuthorizePath,
		"token_endpoint":                                 s.issuer + TokenPath,
		"registration_endpoint":                          s.issuer + RegisterPath,
		"revocation_endpoint":                            s.issuer + RevokePath,
		"scopes_supported":                               []string{Scope},
		"response_types_supported":                       []string{codeParam},
		"grant_types_supported":                          grantTypes,
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{authNone},
		"revocation_endpoint_auth_methods_supported":     []string{authNone},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

// Authenticate returns the live grant of the access token in an Authorization header.
func (s *Service) Authenticate(ctx context.Context, header string) (Grant, error) {
	token, ok := bearer(header)
	if !ok || !strings.HasPrefix(token, AccessPrefix) {
		return Grant{}, ErrInvalidToken
	}
	g, err := s.store.byAccess(ctx, hashHex(token))
	if err != nil {
		return Grant{}, err
	}
	if u, err := s.accounts.Get(g.User); err != nil || u.Disabled {
		return Grant{}, ErrInvalidToken
	}
	if g.UsedAt == nil || time.Since(*g.UsedAt) > usedEvery {
		if err := s.store.touch(ctx, g.ID); err != nil {
			log.WithFunc("oauth.Service.Authenticate").Warnf(ctx, "record the use of grant %s: %v", g.ID, err)
		}
	}
	return g, nil
}

// Boxes returns the boxes grant id created that still exist.
func (s *Service) Boxes(ctx context.Context, id string) ([]string, error) {
	return s.store.grantBoxes(ctx, id)
}

// AddBox records that grant id created box.
func (s *Service) AddBox(ctx context.Context, id, box string) error {
	return s.store.addBox(ctx, id, box)
}

// Grants returns user's grants, newest first.
func (s *Service) Grants(ctx context.Context, user string) ([]Grant, error) {
	return s.store.grants(ctx, user)
}

// Revoke ends user's grant id and its tokens; its boxes stay the user's.
func (s *Service) Revoke(ctx context.Context, user, id string) error {
	return s.store.revoke(ctx, user, id)
}

// MarkSeen records that user saw every grant the console lists.
func (s *Service) MarkSeen(ctx context.Context, user string) error {
	return s.store.markSeen(ctx, user)
}
