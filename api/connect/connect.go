// Package connect is the Connect broker (design §16): sealed grants, OAuth, and the accounts' MCP tools under policy and audit.
package connect

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/singleflight"

	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/keyring"
)

const (
	// Prefix routes the vendor-facing paths: the OAuth callback and the client metadata document.
	Prefix = "/cocoonbox.connect/"
	// CallbackPath is the redirect URI of every OAuth client of the api.
	CallbackPath = Prefix + "callback"
	// ClientMetadataPath serves the api's OAuth client metadata document, whose URL is its client id.
	ClientMetadataPath = Prefix + "client.json"
	// ConsolePath is the console page that connects accounts.
	ConsolePath = "/cb/connect"

	// DoorMCP is mode B: the user's own agents with the user token.
	DoorMCP = "mcp"
	// DoorBox is mode A: the box's agents, which need an approval for every ask tool.
	DoorBox = "box"
	// DoorConsole and DoorApp are where a disconnect comes from.
	DoorConsole = "console"
	DoorApp     = "app"

	statusActive    = "active"
	statusReconnect = "needs_reconnect"

	policyAllow = "allow"
	policyAsk   = "ask"
	policyOff   = "off"

	decisionOnce     = "once"
	decisionAlways   = "always"
	decisionDeny     = "deny"
	decisionUsedNow  = "used"
	decisionApproved = "approved"
	decisionDenied   = "denied"
	decisionRequired = "approval_required"
	decisionDisconn  = "disconnect"

	grantAuthorizationCode = "authorization_code"
	grantRefreshToken      = "refresh_token"
	responseCode           = "code"
	methodInitialize       = "initialize"
	headerAuthorization    = "Authorization"

	outcomeOK      = "ok"
	outcomeError   = "error"
	outcomeUnknown = "outcome_unknown"

	grantKeyInfo   = "cocoonbox connect grant"
	idBytes        = 12
	flowTTL        = 10 * time.Minute
	approvalTTL    = 10 * time.Minute
	refreshMargin  = 5 * time.Minute
	tokenTimeout   = 10 * time.Second
	setupTimeout   = 30 * time.Second
	callTimeout    = 2 * time.Minute
	toolsTTL       = 5 * time.Minute
	auditRetention = 90 * 24 * time.Hour
	purgeInterval  = 24 * time.Hour
)

var (
	errNoGrant    = errors.New("the connector is not connected")
	errNoFlow     = errors.New("the sign-in is unknown, finished or expired; start it again")
	errNoApproval = errors.New("the approval is unknown, answered or expired")
	errReconnect  = errors.New("the vendor no longer accepts the grant")
	errNoDoorKey  = errors.New("no box door key")
	errNoSignIn   = errors.New("the MCP server needs no sign-in")

	connectorPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
	toolNamePattern  = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
)

// Sessions resolves the hash of a console session secret to the enabled user of that live session.
type Sessions interface {
	SessionUser(ctx context.Context, sessionSHA256 string) (string, error)
}

// InjectionFunc runs after a grant of user that injects into boxes is connected, disconnected or lost.
type InjectionFunc func(ctx context.Context, user string)

// Options configures the broker.
type Options struct {
	DSN string
	// Keys gives each user's state key; grants are sealed under a key derived from it.
	Keys     keyring.Keyring
	Sessions Sessions
	// PublicURL is the api's public origin, such as https://box.example.com.
	PublicURL string
	GitHub    Credentials
	Google    Credentials
	HubSpot   Credentials
	Slack     Credentials
	// Link is offered only with all three of its client id, secret and Stripe publishable key.
	Link Credentials
	// AllowPrivate lets the broker reach loopback and private addresses; only tests set it.
	AllowPrivate bool
}

// Service is the broker; it serves Connectors and the MCP doors.
type Service struct {
	controlv1.UnimplementedConnectorsServer

	store        *store
	seal         *sealer
	sessions     Sessions
	catalog      []*entry
	publicURL    string
	http         *http.Client
	allowPrivate bool
	refreshes    singleflight.Group
	watchers     *watchers
	onInjection  InjectionFunc
	linkAPI      string

	mu    sync.Mutex
	conns map[string]*conn

	spendReads singleflight.Group
	spendMu    sync.Mutex
	spends     map[string]*spendPoll
}

// Open connects to the database, applies the Connect migrations and builds the catalog.
func Open(ctx context.Context, opts Options) (*Service, error) {
	st, err := openStore(ctx, opts.DSN)
	if err != nil {
		return nil, err
	}
	return &Service{
		store:        st,
		seal:         &sealer{keyring.Sealer{Keys: opts.Keys, Info: grantKeyInfo}},
		sessions:     opts.Sessions,
		catalog:      newCatalog(opts),
		publicURL:    opts.PublicURL,
		http:         newHTTPClient(opts.AllowPrivate),
		allowPrivate: opts.AllowPrivate,
		watchers:     newWatchers(),
		linkAPI:      linkAPI,
		conns:        map[string]*conn{},
		spends:       map[string]*spendPoll{},
	}, nil
}

// Close releases the database pool.
func (s *Service) Close() { s.store.close() }

// RunPurge deletes audit records older than 90 days, now and then daily, until ctx ends.
func (s *Service) RunPurge(ctx context.Context) {
	logger := log.WithFunc("connect.Service.RunPurge")
	tick := time.Tick(purgeInterval)
	for {
		if n, err := s.store.purge(ctx, auditRetention); err != nil {
			logger.Warnf(ctx, "purge the audit: %v", err)
		} else if n > 0 {
			logger.Infof(ctx, "purged %d audit records older than %s", n, auditRetention)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}

// OnInjection registers fn, which runs synchronously in the call that changed the grant.
func (s *Service) OnInjection(fn InjectionFunc) {
	s.onInjection = fn
}

func (s *Service) entry(id string) *entry {
	if i := slices.IndexFunc(s.catalog, func(e *entry) bool { return e.id == id }); i >= 0 {
		return s.catalog[i]
	}
	return nil
}

func (s *Service) conn(id string) *conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.conns[id]
	if c == nil {
		c = &conn{}
		s.conns[id] = c
	}
	return c
}

func (s *Service) dropConn(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, id)
}

// changed tells the user's open MCP streams and, for a grant that injects, the user's boxes that g changed.
func (s *Service) changed(ctx context.Context, g *grant) {
	s.watchers.notify(g.user)
	if e := s.entry(g.kind); e != nil && len(e.inject) > 0 && s.onInjection != nil {
		s.onInjection(ctx, g.user)
	}
}
