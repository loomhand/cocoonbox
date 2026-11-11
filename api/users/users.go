// Package users keeps cocoonbox users in PostgreSQL, each with one sandboxd tenant and one state key that never leave the api, their boxes and the boxes' shares.
package users

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/sync/errgroup"

	"github.com/loomhand/cocoonbox/api/gwkeys"
	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/utils"
)

const (
	// TokenPrefix starts every API token and device token.
	TokenPrefix  = "cb_"
	EndedChannel = "cocoonbox_ended"

	idPrefix     = "u-"
	idBytes      = 6
	secretBytes  = 32
	sessionTTL   = 12 * time.Hour
	minPassword  = 8
	maxPassword  = 72
	maxBoxesCap  = 16
	defaultBoxes = 1

	tokenMissInterval = 100 * time.Millisecond
	tenantInterval    = 10 * time.Minute
	tenantFanout      = 8
	userStripes       = 256
	listenRetry       = 2 * time.Second
	endedGrace        = 10 * time.Minute

	columnTenant   = "tenant_token"
	columnStateKey = "state_key"

	passwordIDP = "password"
	// githubIDP is signup's provider; an outside provider of that name would reach the users signup made.
	githubIDP = "github"
)

var (
	ErrNotFound    = errors.New("user not found")
	ErrExists      = errors.New("user already exists")
	ErrInvalid     = errors.New("invalid request")
	ErrBadPassword = errors.New("wrong username or password")
	ErrQuota       = errors.New("quota reached")

	namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)
	// idPattern is sandboxd's tenant name grammar, since the id names the tenant.
	idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,62}$`)
	hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Policy is the image a user's boxes start from and upgrade to, whether its saves write delta records, and whether the parking and the gc skip it.
type Policy struct {
	Image  string `json:"image,omitempty"`
	Delta  bool   `json:"delta,omitzero"`
	NoPark bool   `json:"no_park,omitzero"`
	NoGC   bool   `json:"no_gc,omitzero"`
}

// User is a cocoonbox user as the console shows it; it never carries a credential. MaxBoxes and MaxBoxesCap are the limits in force, and MaxRunning 0 is the api's default.
type User struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	IDP         string `json:"idp"`
	Admin       bool   `json:"admin"`
	Disabled    bool   `json:"disabled"`
	Residential bool   `json:"residential"`
	MaxBoxes    int    `json:"max_boxes"`
	MaxBoxesCap int    `json:"max_boxes_cap"`
	MaxRunning  int    `json:"max_running"`
	// MaxAgentBoxes bounds the boxes all of the user's agent grants hold together; nil leaves only each grant's caps.
	MaxAgentBoxes *int      `json:"max_agent_boxes"`
	HasToken      bool      `json:"has_token"`
	CreatedAt     time.Time `json:"created_at"`
	Policy        Policy    `json:"policy"`
}

// from returns u when outside identity provider idp holds it.
func (u User) from(idp string) (User, error) {
	if u.IDP != idp {
		return User{}, fmt.Errorf("%w: user %s comes from %s", ErrExists, u.ID, u.IDP)
	}
	return u, nil
}

// NewUser describes a user to create; an empty ID draws one, an empty Password leaves password sign-in off, and a nil DailyCapMicros keeps the gw default.
type NewUser struct {
	ID             string
	Name           string
	Password       string
	Admin          bool
	IDP            string
	MaxAgentBoxes  *int
	DailyCapMicros *int64
}

// Settings changes a user's settings: nil keeps a value, MaxBoxes, MaxBoxesCap and MaxRunning 0 are the api defaults, MaxAgentBoxes 0 no cap, and Admin lifts the gw cap while set.
type Settings struct {
	Admin         *bool `json:"admin,omitempty"`
	Disabled      *bool `json:"disabled,omitempty"`
	Residential   *bool `json:"residential,omitempty"`
	MaxBoxes      *int  `json:"max_boxes,omitempty"`
	MaxBoxesCap   *int  `json:"max_boxes_cap,omitempty"`
	MaxRunning    *int  `json:"max_running,omitempty"`
	MaxAgentBoxes *int  `json:"max_agent_boxes,omitempty"`

	Image  *string `json:"image,omitempty"`
	Delta  *bool   `json:"delta,omitempty"`
	NoPark *bool   `json:"no_park,omitempty"`
	NoGC   *bool   `json:"no_gc,omitempty"`
}

// GWKeys issues, revokes and meters each user's gw keys and keeps the user's own daily cap, found by owner = user id.
type GWKeys interface {
	AddMachine(ctx context.Context, owner string, enabled bool) (string, string, error)
	Delete(ctx context.Context, id string) error
	Revoke(ctx context.Context, owner string) error
	SetEnabled(ctx context.Context, owner string, enabled bool) error
	Usage(ctx context.Context, owner string, since time.Time) ([]gwkeys.Usage, error)
	DailyCap(ctx context.Context, owner string) (*int64, error)
	DailyCaps(ctx context.Context) (map[string]int64, error)
	SetDailyCap(ctx context.Context, owner string, micros *int64) error
}

// Options names the database, the master key file and the backends that hold each user's keys.
type Options struct {
	DSN           string
	MasterKeyFile string
	GW            GWKeys
	Tenants       machines.Tenants
	// TenantClaims makes Tenant hand out each user's tenant token, so machines claim under it.
	TenantClaims bool
	// FirstBoxID names the box Create gives each new user.
	FirstBoxID func(user string) string
	// BoxFlavor is the flavor every new box takes.
	BoxFlavor machines.Flavor
	// EnsureLimit sizes the user lock pool, which bounds the creates Ensure runs at once.
	EnsureLimit int32
	// PasswordCost is the bcrypt cost of new passwords and of the unknown-name check; 0 is bcrypt.DefaultCost.
	PasswordCost int
	// MaxBoxes is how many boxes a user may have while the user's row sets none; 0 is 1.
	MaxBoxes int
}

// record is a user row with the values the console never sees.
type record struct {
	User
	tokenSHA256 string
	devices     []deviceToken
	tenantToken []byte
	stateKeyID  string
	stateKey    []byte
}

type life struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type credLife struct {
	life
	refs int
}

// tokenOwner is the user a token serves and, for a device token, its device id.
type tokenOwner struct {
	user   string
	device string
}

type grantOwner func(credential string) bool

type liveGrants func(ctx context.Context, ids []string) (map[string]bool, error)

// Service owns the users, boxes and shares tables and an in-memory index that serves every token, box and share check.
type Service struct {
	store        *store
	seal         *sealer
	gw           GWKeys
	tenants      machines.Tenants
	tenantClaims bool
	firstBoxID   func(string) string
	boxFlavor    machines.Flavor
	nextMiss     atomic.Int64
	// dummyHash keeps a sign-in for an unknown name as slow as one for a known name.
	dummyHash    []byte
	passwordCost int
	stripes      [userStripes]sync.Mutex

	mu           sync.RWMutex
	byID         map[string]record
	byToken      map[string]tokenOwner
	seq          uint64
	touched      map[string]uint64
	loading      int
	loadStart    uint64
	deviceUse    sync.Map
	boxes        map[string][]Box
	shares       map[string]*shareRecord
	shareSecrets map[string]string
	// shareGen counts this api's share changes, so a sync read before one does not undo it.
	shareGen      int
	onResidential []func(context.Context, string, bool)
	onBoxes       []func(owner string)
	onShareEnd    []func(context.Context, Share)

	lifeMu     sync.Mutex
	lives      map[string]life
	creds      map[string]*credLife
	tombs      map[string]time.Time
	grantsOwn  grantOwner
	grantsLive liveGrants
}

// Open connects to the database, applies migrations and loads every user.
func Open(ctx context.Context, opts Options) (*Service, error) {
	seal, err := newSealer(opts.MasterKeyFile)
	if err != nil {
		return nil, err
	}
	maxBoxes := cmp.Or(opts.MaxBoxes, defaultBoxes)
	if maxBoxes > maxBoxesCap {
		return nil, fmt.Errorf("max boxes %d is past the %d a user may have", maxBoxes, maxBoxesCap)
	}
	st, err := openStore(ctx, opts.DSN, opts.EnsureLimit, maxBoxes)
	if err != nil {
		return nil, err
	}
	cost := cmp.Or(opts.PasswordCost, bcrypt.DefaultCost)
	dummy, err := bcrypt.GenerateFromPassword([]byte(utils.RandomHex(secretBytes)), cost)
	if err != nil {
		st.close()
		return nil, err
	}
	s := &Service{
		store: st, seal: seal, gw: opts.GW, tenants: opts.Tenants, tenantClaims: opts.TenantClaims, firstBoxID: opts.FirstBoxID, boxFlavor: opts.BoxFlavor, dummyHash: dummy, passwordCost: cost,
		boxes: map[string][]Box{}, touched: map[string]uint64{}, shares: map[string]*shareRecord{}, shareSecrets: map[string]string{}, lives: map[string]life{},
		creds: map[string]*credLife{}, tombs: map[string]time.Time{},
	}
	if err := s.load(ctx); err != nil {
		st.close()
		return nil, err
	}
	if err := s.loadShares(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close stops the share expiry timers and releases the database pool.
func (s *Service) Close() {
	s.mu.Lock()
	for _, rec := range s.shares {
		rec.timer.Stop()
	}
	s.mu.Unlock()
	s.store.close()
}

func (s *Service) SyncUser(ctx context.Context, id string) error {
	hooks, cut, err := s.installUser(ctx, id)
	if err != nil {
		return err
	}
	for _, fn := range hooks {
		fn(id)
	}
	if cut {
		return s.endGoneGrants(ctx)
	}
	return nil
}

// Listen reads the one user or the shares a users notify names, reloads the index and shares on any other notify and after each LISTEN, which misses earlier notifies, and ends credentials other apis ended, until ctx ends.
func (s *Service) Listen(ctx context.Context) {
	for {
		err := s.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		log.WithFunc("users.Service.Listen").Warnf(ctx, "reload users on notify: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(listenRetry):
		}
	}
}

// RunTenants rewrites, until ctx ends and every tenantInterval, the tenants sandboxd lacks or holds with another cap or egress class.
func (s *Service) RunTenants(ctx context.Context) {
	logger := log.WithFunc("users.Service.RunTenants")
	tick := time.Tick(tenantInterval)
	for {
		n, err := s.reconcileTenants(ctx)
		if n > 0 {
			logger.Warnf(ctx, "rewrote %d stale sandboxd tenants", n)
		}
		if err != nil {
			logger.Warnf(ctx, "reconcile the sandboxd tenants: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}

// ByTokenSHA256 returns the enabled user whose API or device token hashes to sum; a miss rechecks the tables at most once per tokenMissInterval.
func (s *Service) ByTokenSHA256(ctx context.Context, sum string) (string, bool) {
	owner, ok := s.indexedToken(sum)
	if !ok && s.allowMiss() {
		if user, err := s.store.tokenOwner(ctx, sum); err == nil && user != "" && s.SyncUser(ctx, user) == nil {
			owner, ok = s.indexedToken(sum)
		}
	}
	if ok && owner.device != "" {
		s.touchDevice(ctx, owner.device)
	}
	return owner.user, ok
}

// Get returns user id.
func (s *Service) Get(id string) (User, error) {
	r, err := s.record(id)
	if err != nil {
		return User{}, err
	}
	return s.user(r), nil
}

// ByName returns the user signed in as name.
func (s *Service) ByName(name string) (User, error) {
	for _, r := range s.records() {
		if r.Name == name {
			return s.user(r), nil
		}
	}
	return User{}, ErrNotFound
}

// List returns every user ordered by name.
func (s *Service) List() []User {
	records := s.records()
	out := make([]User, 0, len(records))
	for _, r := range records {
		out = append(out, s.user(r))
	}
	return out
}

// Create adds a user with its first box and tenant and sets its gw cap, unlimited for an admin; a failure leaves none of them behind.
func (s *Service) Create(ctx context.Context, nu NewUser) (User, error) {
	nu.ID = cmp.Or(nu.ID, idPrefix+utils.RandomHex(idBytes))
	if !namePattern.MatchString(nu.Name) {
		return User{}, fmt.Errorf("%w: a username is 2 to 32 lowercase letters, digits, - or _, starting with a letter", ErrInvalid)
	}
	return s.create(ctx, nu)
}

// Ensure returns the user outside identity provider idp names id, creating it as Create does, never an admin, the first time; an id another source holds is ErrExists.
func (s *Service) Ensure(ctx context.Context, idp, id string) (User, error) {
	if err := outsideUser(idp, id); err != nil {
		return User{}, err
	}
	if u, err := s.Get(id); err == nil {
		return u.from(idp)
	}
	return s.createOnce(ctx, idp, id)
}

// Delete removes user id's tenant, gw key and cap, sessions, boxes and row; the caller releases the boxes' machines first.
func (s *Service) Delete(ctx context.Context, id string) error {
	if _, err := s.record(id); err != nil {
		return err
	}
	if err := s.tenants.DeleteTenant(ctx, id); err != nil {
		return fmt.Errorf("delete tenant: %w", err)
	}
	if err := s.gw.Revoke(ctx, id); err != nil {
		return fmt.Errorf("revoke gw key: %w", err)
	}
	if err := s.gw.SetDailyCap(ctx, id, nil); err != nil {
		return fmt.Errorf("drop gw cap: %w", err)
	}
	if err := s.store.remove(ctx, id); err != nil {
		return err
	}
	defer s.editUser(id)()
	if r, ok := s.byID[id]; ok {
		dropTokens(s.byToken, r)
		delete(s.byID, id)
	}
	delete(s.boxes, id)
	s.cutOwner(id, true)
	s.end(id)
	return nil
}

// Update applies set to user id; the tenant's cap follows MaxBoxes, and a disabled user's gw keys and open streams stop.
func (s *Service) Update(ctx context.Context, id string, set Settings) (User, error) {
	r, err := s.record(id)
	if err != nil {
		return User{}, err
	}
	old := r
	if set.MaxBoxesCap != nil {
		r.MaxBoxesCap = *set.MaxBoxesCap
		if boxes, limit := s.boxLimits(r); boxes > limit {
			r.MaxBoxes = r.MaxBoxesCap
		}
	}
	if set.MaxBoxes != nil {
		r.MaxBoxes = *set.MaxBoxes
	}
	r.MaxRunning = deref(set.MaxRunning, r.MaxRunning)
	if set.MaxAgentBoxes != nil {
		r.MaxAgentBoxes = nil
		if *set.MaxAgentBoxes != 0 {
			r.MaxAgentBoxes = new(*set.MaxAgentBoxes)
		}
	}
	r.Admin = deref(set.Admin, r.Admin)
	r.Disabled = deref(set.Disabled, r.Disabled)
	r.Residential = deref(set.Residential, r.Residential)
	r.Policy.Image = deref(set.Image, r.Policy.Image)
	r.Policy.Delta = deref(set.Delta, r.Policy.Delta)
	r.Policy.NoPark = deref(set.NoPark, r.Policy.NoPark)
	r.Policy.NoGC = deref(set.NoGC, r.Policy.NoGC)
	own := len(s.Boxes(id)) - s.agentBoxes(id)
	boxes, limit := s.boxLimits(r)
	oldBoxes, _ := s.boxLimits(old)
	switch {
	case r.MaxBoxesCap < 0 || limit > maxBoxesCap:
		return User{}, fmt.Errorf("%w: max boxes cap must be in 1..%d, or 0 for the default", ErrInvalid, maxBoxesCap)
	case r.MaxBoxes < 0 || boxes > limit:
		return User{}, fmt.Errorf("%w: max boxes must be in 1..%d, or 0 for the default", ErrInvalid, limit)
	case boxes < oldBoxes && boxes < own:
		return User{}, fmt.Errorf("%w: the user has %d boxes; delete boxes before lowering max boxes", ErrInvalid, own)
	case r.MaxRunning < 0 || r.MaxRunning > boxes:
		return User{}, fmt.Errorf("%w: the running limit must be in 1..%d (max boxes), or 0 for the default; lower it before max boxes", ErrInvalid, boxes)
	case r.MaxAgentBoxes != nil && (*r.MaxAgentBoxes < 1 || *r.MaxAgentBoxes > maxBoxesCap):
		return User{}, fmt.Errorf("%w: max agent boxes must be in 1..%d, or 0 for no cap", ErrInvalid, maxBoxesCap)
	}
	if err := s.moveAdmin(ctx, old, r); err != nil {
		return User{}, err
	}
	if s.claimCap(r) != s.claimCap(old) {
		if err := s.tenants.PutTenant(ctx, id, "", s.claimCap(r)); err != nil {
			return User{}, fmt.Errorf("set tenant cap: %w", err)
		}
	}
	if r.Disabled != old.Disabled {
		if err := s.gw.SetEnabled(ctx, id, !r.Disabled); err != nil {
			return User{}, fmt.Errorf("set gw key: %w", err)
		}
	}
	if err := s.store.update(ctx, r, set); err != nil {
		return User{}, err
	}
	if r.Disabled && !old.Disabled {
		if err := s.store.removeSessions(ctx, id); err != nil {
			return User{}, err
		}
	}
	if err := s.SyncUser(ctx, id); err != nil {
		return User{}, err
	}
	if r.Residential != old.Residential {
		s.mu.RLock()
		hooks := slices.Clone(s.onResidential)
		s.mu.RUnlock()
		for _, fn := range hooks {
			fn(ctx, id, r.Residential)
		}
	}
	return s.Get(id)
}

// SetPassword replaces user id's password and ends its console sessions.
func (s *Service) SetPassword(ctx context.Context, id, password string) error {
	if _, err := s.record(id); err != nil {
		return err
	}
	hash, err := hashPassword(password, s.passwordCost)
	if err != nil {
		return err
	}
	if err := s.store.setPassword(ctx, id, string(hash)); err != nil {
		return err
	}
	return s.store.removeSessions(ctx, id)
}

// SetTokenSHA256 makes the token hashing to sum user id's API token.
func (s *Service) SetTokenSHA256(ctx context.Context, id, sum string) error {
	if !hexSHA256.MatchString(sum) {
		return fmt.Errorf("%w: a token sha256 is 64 lowercase hex digits", ErrInvalid)
	}
	r, err := s.record(id)
	if err != nil {
		return err
	}
	if err := s.store.setToken(ctx, id, sum); err != nil {
		return err
	}
	if r.tokenSHA256 != sum {
		s.EndCredential(ctx, r.tokenSHA256)
	}
	return s.SyncUser(ctx, id)
}

// RegenerateToken gives user id a new API token, returned once; the old one stops working at once.
func (s *Service) RegenerateToken(ctx context.Context, id string) (string, error) {
	token := TokenPrefix + utils.RandomHex(secretBytes)
	if err := s.SetTokenSHA256(ctx, id, utils.HashHex(token)); err != nil {
		return "", err
	}
	return token, nil
}

// RevokeToken drops user id's API token at once, with the calls it holds open; a user without one is no error.
func (s *Service) RevokeToken(ctx context.Context, id string) error {
	r, err := s.record(id)
	if err != nil {
		return err
	}
	if err := s.store.clearToken(ctx, id); err != nil {
		return err
	}
	s.EndCredential(ctx, r.tokenSHA256)
	return s.SyncUser(ctx, id)
}

// SignOut ends every sign-in of user id at once: its console sessions, personal token and device tokens, and every call open on them.
func (s *Service) SignOut(ctx context.Context, id string) error {
	if _, err := s.record(id); err != nil {
		return err
	}
	if err := s.store.signOut(ctx, id); err != nil {
		return err
	}
	unlock := s.editUser(id)
	if r, ok := s.byID[id]; ok {
		dropTokens(s.byToken, r)
		for _, d := range r.devices {
			s.endCredential(d.sha256)
			s.deviceUse.Delete(d.id)
		}
		s.endCredential(r.tokenSHA256)
		r.tokenSHA256, r.HasToken, r.devices = "", false, nil
		s.byID[id] = r
	}
	s.end(id)
	unlock()
	return nil
}

// Login checks name's password and returns the user.
func (s *Service) Login(ctx context.Context, name, password string) (User, error) {
	id, hash, err := s.store.password(ctx, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return User{}, err
	}
	if id == "" || hash == "" {
		_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
		return User{}, ErrBadPassword
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return User{}, ErrBadPassword
	}
	if u, err := s.Get(id); !errors.Is(err, ErrNotFound) {
		return u, err
	}
	if err := s.SyncUser(ctx, id); err != nil {
		return User{}, err
	}
	return s.Get(id)
}

// NewSession starts a console session for user id and returns its secret.
func (s *Service) NewSession(ctx context.Context, id string) (string, error) {
	secret := utils.RandomHex(secretBytes)
	if err := s.store.addSession(ctx, utils.HashHex(secret), id, time.Now().Add(sessionTTL)); err != nil {
		return "", err
	}
	return secret, nil
}

// Session returns the enabled user of the session with secret.
func (s *Service) Session(ctx context.Context, secret string) (User, error) {
	id, err := s.SessionUser(ctx, utils.HashHex(secret))
	if err != nil {
		return User{}, err
	}
	return s.Get(id)
}

// SessionUser returns the id of the enabled user of the live session whose secret hashes to sum.
func (s *Service) SessionUser(ctx context.Context, sum string) (string, error) {
	id, err := s.store.session(ctx, sum)
	if err != nil {
		return "", err
	}
	u, err := s.Get(id)
	if err == nil && u.Disabled {
		return "", ErrNotFound
	}
	return u.ID, err
}

// EndSession ends the session with secret.
func (s *Service) EndSession(ctx context.Context, secret string) error {
	return s.store.removeSession(ctx, utils.HashHex(secret))
}

// Usage returns user id's gw usage by model since since.
func (s *Service) Usage(ctx context.Context, id string, since time.Time) ([]gwkeys.Usage, error) {
	if _, err := s.record(id); err != nil {
		return nil, err
	}
	return s.gw.Usage(ctx, id, since)
}

// DailyCap returns user id's own gw daily cost cap in micros or gwkeys.Unlimited; nil means the tenant default.
func (s *Service) DailyCap(ctx context.Context, id string) (*int64, error) {
	if _, err := s.record(id); err != nil {
		return nil, err
	}
	return s.gw.DailyCap(ctx, id)
}

// DailyCaps returns the gw daily cost cap of every user that has its own.
func (s *Service) DailyCaps(ctx context.Context) (map[string]int64, error) {
	return s.gw.DailyCaps(ctx)
}

// SetDailyCap gives user id its own gw daily cost cap in micros or gwkeys.Unlimited; nil puts it back on the tenant default.
func (s *Service) SetDailyCap(ctx context.Context, id string, micros *int64) error {
	if micros != nil && *micros < 0 && *micros != gwkeys.Unlimited {
		return fmt.Errorf("%w: a daily cap is at least 0 micros, or unlimited", ErrInvalid)
	}
	if _, err := s.record(id); err != nil {
		return err
	}
	return s.gw.SetDailyCap(ctx, id, micros)
}

// Tenant returns user id's sandboxd tenant token, empty while machines stay root claims.
func (s *Service) Tenant(id string) (string, error) {
	r, err := s.record(id)
	if err != nil || !s.tenantClaims {
		return "", err
	}
	return s.seal.open(r.tenantToken, userData(id, columnTenant))
}

// StateKey returns user id's state key.
func (s *Service) StateKey(_ context.Context, id string) (keyring.Key, error) {
	r, err := s.record(id)
	if err != nil {
		return keyring.Key{}, err
	}
	secret, err := s.seal.open(r.stateKey, userData(id, columnStateKey))
	if err != nil {
		return keyring.Key{}, err
	}
	return keyring.Key{ID: r.stateKeyID, Secret: []byte(secret)}, nil
}

// Policy returns user id's policy; an unknown user has the zero one.
func (s *Service) Policy(id string) Policy {
	r, err := s.record(id)
	if err != nil {
		return Policy{}
	}
	return r.Policy
}

func (s *Service) Disabled(id string) bool {
	r, err := s.record(id)
	return err == nil && r.Disabled
}

func (s *Service) AccountDisabled(ctx context.Context, id string) (bool, error) {
	return s.store.disabled(ctx, id)
}

// MaxRunning returns how many of user id's boxes may run at once, 0 when the user takes the api's default.
func (s *Service) MaxRunning(id string) int {
	r, err := s.record(id)
	if err != nil {
		return 0
	}
	return r.MaxRunning
}

// Residential reports whether user id's boxes take the residential exit.
func (s *Service) Residential(id string) bool {
	r, err := s.record(id)
	return err == nil && r.Residential
}

func (s *Service) Lifetime(id, credential string) (context.Context, func()) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, tomb := s.tombs[credential]
	if r, ok := s.byID[id]; !ok || r.Disabled || tomb || hexSHA256.MatchString(credential) && s.byToken[credential].user != id {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx, func() {}
	}
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	user := liveIn(context.Background(), s.lives, id)
	if credential == "" {
		return user, func() {}
	}
	c := s.creds[credential]
	if c == nil || c.ctx.Err() != nil {
		c = &credLife{}
		c.ctx, c.cancel = context.WithCancel(user)
		s.creds[credential] = c
	}
	c.refs++
	return c.ctx, func() { s.release(credential, c) }
}

// EndCredential ends the calls bound to credential, a token's sha256 or a grant id, in this api; the write that ended it tells the others, and an ended grant id stays ended.
func (s *Service) EndCredential(_ context.Context, credential string) {
	s.endLocally(credential)
}

// KeepGrants registers live, which reports which of the grant ids owns names still exist, so a reconnecting listener ends the calls of the rest.
func (s *Service) KeepGrants(owns grantOwner, live liveGrants) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grantsOwn, s.grantsLive = owns, live
}

// OnResidential registers fn to run after a user's residential setting changes.
func (s *Service) OnResidential(fn func(ctx context.Context, id string, on bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onResidential = append(s.onResidential, fn)
}

func (s *Service) OnBoxes(fn func(owner string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onBoxes = append(s.onBoxes, fn)
}

// createOnce creates outside user id under its lock unless another api created it meanwhile.
func (s *Service) createOnce(ctx context.Context, idp, id string) (User, error) {
	// a create that loses a race rolls back the tenant and daily cap the winner holds, so creates of one id take turns
	unlock, err := s.store.lockUser(ctx, id)
	if err != nil {
		return User{}, err
	}
	defer unlock()
	exists, err := s.store.exists(ctx, id)
	switch {
	case err != nil:
		return User{}, fmt.Errorf("look up user %s: %w", id, err)
	case !exists:
		return s.create(ctx, NewUser{ID: id, Name: id, IDP: idp})
	}
	if err = s.SyncUser(ctx, id); err != nil {
		return User{}, err
	}
	u, err := s.Get(id)
	if err != nil {
		return User{}, err
	}
	return u.from(idp)
}

func (s *Service) create(ctx context.Context, nu NewUser) (User, error) {
	id := nu.ID
	hash, err := s.admissible(nu)
	if err != nil {
		return User{}, err
	}
	tenantToken := utils.RandomHex(secretBytes)
	stateKey := keyring.Generate(id)
	r := record{
		ID:            id,
		Name:          nu.Name,
		IDP:           cmp.Or(nu.IDP, passwordIDP),
		Admin:         nu.Admin,
		MaxAgentBoxes: nu.MaxAgentBoxes,
		CreatedAt:     time.Now(),
		tenantToken:   s.seal.seal(tenantToken, userData(id, columnTenant)),
		stateKeyID:    stateKey.ID,
		stateKey:      s.seal.seal(string(stateKey.Secret), userData(id, columnStateKey)),
	}
	first := Box{ID: s.firstBoxID(id), Owner: id, Name: FirstBoxName, CreatedAt: r.CreatedAt.Truncate(time.Microsecond), Flavor: s.boxFlavor}
	if err = s.persist(ctx, nu, r, tenantToken, hash, first); err != nil {
		return User{}, errors.Join(err, s.gw.SetDailyCap(context.WithoutCancel(ctx), id, nil))
	}
	if err := s.SyncUser(ctx, id); err != nil {
		return User{}, err
	}
	return s.Get(id)
}

// admissible checks nu's id and caps, that neither its name nor its id is taken, and hashes its password.
func (s *Service) admissible(nu NewUser) ([]byte, error) {
	switch {
	case !idPattern.MatchString(nu.ID):
		return nil, fmt.Errorf("%w: id %q is not a sandboxd tenant name", ErrInvalid, nu.ID)
	case nu.MaxAgentBoxes != nil && (*nu.MaxAgentBoxes < 1 || *nu.MaxAgentBoxes > maxBoxesCap):
		return nil, fmt.Errorf("%w: max agent boxes must be in 1..%d", ErrInvalid, maxBoxesCap)
	}
	var hash []byte
	if nu.Password != "" {
		var err error
		if hash, err = hashPassword(nu.Password, s.passwordCost); err != nil {
			return nil, err
		}
	}
	if _, err := s.ByName(nu.Name); err == nil {
		return nil, ErrExists
	}
	if _, err := s.record(nu.ID); err == nil {
		return nil, ErrExists
	}
	return hash, nil
}

// persist sets the new user's gw cap, puts its tenant and inserts its row and first box; the tenant goes again when the insert fails.
func (s *Service) persist(ctx context.Context, nu NewUser, r record, tenantToken string, hash []byte, first Box) error {
	var err error
	switch {
	case nu.Admin:
		err = s.gw.SetDailyCap(ctx, r.ID, new(int64(gwkeys.Unlimited)))
	case nu.DailyCapMicros != nil:
		err = s.gw.SetDailyCap(ctx, r.ID, nu.DailyCapMicros)
	}
	if err != nil {
		return err
	}
	if err = s.tenants.PutTenant(ctx, r.ID, tenantToken, s.claimCap(r)); err != nil {
		return err
	}
	if err = s.store.insert(ctx, r, string(hash), first); err != nil {
		return errors.Join(err, s.tenants.DeleteTenant(context.WithoutCancel(ctx), r.ID))
	}
	return nil
}

// endLocally ends the calls bound to credential in this api, for an ended credential of any api.
func (s *Service) endLocally(credential string) {
	token := hexSHA256.MatchString(credential)
	if owner, ok := s.indexedToken(credential); ok && token {
		stripe := s.stripe(owner.user)
		stripe.Lock()
		defer stripe.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endCredential(credential)
	if token {
		if owner, ok := s.byToken[credential]; ok {
			s.touch(owner.user)
		}
		delete(s.byToken, credential)
	}
	if credential == "" || token {
		return
	}
	now := time.Now()
	maps.DeleteFunc(s.tombs, func(_ string, at time.Time) bool { return now.Sub(at) > endedGrace })
	s.tombs[credential] = now
}

func (s *Service) listen(ctx context.Context) error {
	conn, err := s.store.listen(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	reload := true
	for {
		if reload {
			if err := s.load(ctx); err != nil {
				return err
			}
			if err := s.syncShares(ctx); err != nil {
				return err
			}
			if err := s.endGoneGrants(ctx); err != nil {
				return err
			}
		}
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		reload = false
		user, single := userOf(n.Payload)
		switch {
		case n.Channel == EndedChannel:
			s.endLocally(n.Payload)
		case n.Payload == sharesPayload:
			err = s.syncShares(ctx)
		case single:
			err = s.SyncUser(ctx, user)
		default:
			reload = true
		}
		if err != nil {
			return err
		}
	}
}

// load replaces the in-memory index with a current snapshot of the tables.
func (s *Service) load(ctx context.Context) error {
	s.mu.Lock()
	s.seq++
	start := s.seq
	s.loading++
	s.mu.Unlock()
	byID, byToken, byOwner, err := s.readIndex(ctx)
	s.mu.Lock()
	s.loading--
	if err != nil || s.loadStart > start {
		s.settleTouched()
		s.mu.Unlock()
		return err
	}
	s.loadStart = start
	s.keepNewer(start, byID, byToken, byOwner)
	s.settleTouched()
	var changed []string
	for id, r := range byID {
		if r.Disabled && !s.byID[id].Disabled {
			s.cutOwner(id, false)
		}
	}
	s.byID, s.byToken = byID, byToken
	for owner := range byOwner {
		if !slices.Equal(s.boxes[owner], byOwner[owner]) {
			changed = append(changed, owner)
		}
	}
	for owner := range s.boxes {
		if _, ok := byOwner[owner]; !ok {
			changed = append(changed, owner)
		}
	}
	s.boxes = byOwner
	for id := range s.lives {
		if r, ok := byID[id]; !ok || r.Disabled {
			s.end(id)
		}
	}
	for credential := range s.creds {
		if _, ok := byToken[credential]; !ok && hexSHA256.MatchString(credential) {
			s.endCredential(credential)
		}
	}
	hooks := slices.Clone(s.onBoxes)
	s.mu.Unlock()
	for _, owner := range changed {
		for _, fn := range hooks {
			fn(owner)
		}
	}
	return nil
}

func (s *Service) installUser(ctx context.Context, id string) ([]func(string), bool, error) {
	stripe := s.stripe(id)
	stripe.Lock()
	defer stripe.Unlock()
	s.mu.Lock()
	s.seq++
	begin := s.seq
	s.mu.Unlock()
	r, boxes, err := s.store.one(ctx, id)
	gone := errors.Is(err, ErrNotFound)
	if err != nil && !gone {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadStart > begin {
		return nil, false, nil
	}
	old, known := s.byID[id]
	dropTokens(s.byToken, old)
	if gone {
		delete(s.byID, id)
	} else {
		s.byID[id] = r
		addTokens(s.byToken, r)
	}
	release := func(sum string) {
		if _, ok := s.byToken[sum]; !ok {
			s.endCredential(sum)
		}
	}
	release(old.tokenSHA256)
	for _, d := range old.devices {
		release(d.sha256)
	}
	switch {
	case gone && known:
		s.cutOwner(id, true)
	case !gone && r.Disabled && !old.Disabled:
		s.cutOwner(id, false)
	}
	if gone || r.Disabled {
		s.end(id)
	}
	var hooks []func(string)
	if !slices.Equal(s.boxes[id], boxes) {
		if len(boxes) == 0 {
			delete(s.boxes, id)
		} else {
			s.boxes[id] = boxes
		}
		hooks = slices.Clone(s.onBoxes)
	}
	if s.loading > 0 {
		s.touched[id] = begin
	}
	return hooks, known && (gone || r.Disabled && !old.Disabled), nil
}

func (s *Service) readIndex(ctx context.Context) (map[string]record, map[string]tokenOwner, map[string][]Box, error) {
	records, err := s.store.all(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	devices, err := s.store.allDevices(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	boxes, err := s.store.allBoxes(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	byOwner := map[string][]Box{}
	for _, b := range boxes {
		byOwner[b.Owner] = append(byOwner[b.Owner], b)
	}
	byID := make(map[string]record, len(records))
	byToken := make(map[string]tokenOwner, len(records))
	for _, r := range records {
		r.devices = devices[r.ID]
		byID[r.ID] = r
		addTokens(byToken, r)
	}
	return byID, byToken, byOwner, nil
}

func (s *Service) keepNewer(start uint64, byID map[string]record, byToken map[string]tokenOwner, byOwner map[string][]Box) {
	keep := map[string]bool{}
	for id, at := range s.touched {
		if at > start {
			keep[id] = true
		}
	}
	if len(keep) == 0 {
		return
	}
	for id := range keep {
		dropTokens(byToken, byID[id])
		cur, ok := s.byID[id]
		if ok {
			byID[id] = cur
		} else {
			delete(byID, id)
		}
		if b, ok := s.boxes[id]; ok {
			byOwner[id] = b
		} else {
			delete(byOwner, id)
		}
		kept := func(sum string) {
			if owner, ok := s.byToken[sum]; ok && owner.user == id {
				byToken[sum] = owner
			}
		}
		kept(cur.tokenSHA256)
		for _, d := range cur.devices {
			kept(d.sha256)
		}
	}
}

func (s *Service) settleTouched() {
	if s.loading == 0 {
		clear(s.touched)
	}
}

func (s *Service) editUser(id string) func() {
	stripe := s.stripe(id)
	stripe.Lock()
	s.mu.Lock()
	s.touch(id)
	return func() {
		s.mu.Unlock()
		stripe.Unlock()
	}
}

func (s *Service) touch(id string) {
	s.seq++
	if s.loading > 0 {
		s.touched[id] = s.seq
	}
}

func (s *Service) stripe(id string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return &s.stripes[h.Sum32()%userStripes]
}

func (s *Service) endGoneGrants(ctx context.Context) error {
	s.mu.RLock()
	s.lifeMu.Lock()
	owns, live := s.grantsOwn, s.grantsLive
	var ids []string
	for credential := range s.creds {
		if owns != nil && owns(credential) {
			ids = append(ids, credential)
		}
	}
	s.lifeMu.Unlock()
	s.mu.RUnlock()
	if live == nil || len(ids) == 0 {
		return nil
	}
	alive, err := live(ctx, ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !alive[id] {
			s.endLocally(id)
		}
	}
	return nil
}

func (s *Service) record(id string) (record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byID[id]
	if !ok {
		return record{}, ErrNotFound
	}
	return r, nil
}

// user is r as the console shows it, with the limits in force.
func (s *Service) user(r record) User {
	u := r.User
	u.MaxBoxes, u.MaxBoxesCap = s.boxLimits(r)
	return u
}

func (s *Service) indexedToken(sum string) (tokenOwner, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owner, ok := s.byToken[sum]
	return owner, ok
}

func (s *Service) allowMiss() bool {
	now, next := time.Now().UnixNano(), s.nextMiss.Load()
	return now >= next && s.nextMiss.CompareAndSwap(next, now+int64(tokenMissInterval))
}

// moveAdmin refuses demoting the last enabled admin and gives a promoted user no gw cap, a demoted one the tenant's default.
func (s *Service) moveAdmin(ctx context.Context, old, r record) error {
	if r.Admin == old.Admin {
		return nil
	}
	var daily *int64
	if r.Admin {
		daily = new(int64(gwkeys.Unlimited))
	} else if !slices.ContainsFunc(s.records(), func(o record) bool { return o.Admin && !o.Disabled && o.ID != r.ID }) {
		return fmt.Errorf("%w: the last admin stays an admin", ErrInvalid)
	}
	if err := s.gw.SetDailyCap(ctx, r.ID, daily); err != nil {
		return fmt.Errorf("set gw cap: %w", err)
	}
	return nil
}

func (s *Service) end(id string) {
	if l, ok := s.lives[id]; ok {
		l.cancel()
		delete(s.lives, id)
	}
}

func (s *Service) endCredential(credential string) {
	if c, ok := s.creds[credential]; ok {
		c.cancel()
		delete(s.creds, credential)
	}
}

func (s *Service) release(credential string, c *credLife) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if c.refs--; c.refs == 0 && s.creds[credential] == c {
		c.cancel()
		delete(s.creds, credential)
	}
}

func (s *Service) records() []record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.SortedFunc(maps.Values(s.byID), func(a, b record) int { return strings.Compare(a.Name, b.Name) })
}

// reconcileTenants writes each stale tenant from the user's current record, skipping users deleted meanwhile, and returns how many it wrote.
func (s *Service) reconcileTenants(ctx context.Context) (int, error) {
	records := s.records()
	want := make(map[string]int, len(records))
	for _, r := range records {
		want[r.ID] = s.claimCap(r)
	}
	stale, err := s.tenants.StaleTenants(ctx, want)
	if err != nil {
		return 0, err
	}
	errs := make([]error, len(stale))
	var written atomic.Int32
	var g errgroup.Group
	g.SetLimit(tenantFanout)
	for i, id := range stale {
		g.Go(func() error {
			r, err := s.record(id)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err == nil {
				var token string
				if token, err = s.seal.open(r.tenantToken, userData(id, columnTenant)); err == nil {
					err = s.tenants.PutTenant(ctx, id, token, s.claimCap(r))
				}
			}
			if err != nil {
				errs[i] = fmt.Errorf("tenant of %s: %w", id, err)
				return nil
			}
			written.Add(1)
			return nil
		})
	}
	_ = g.Wait()
	return int(written.Load()), errors.Join(errs...)
}

// outsideUser checks that idp is an outside identity provider, not cocoonbox's own, and names a user id.
func outsideUser(idp, id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: an outside user needs its id", ErrInvalid)
	case idp == "" || idp == passwordIDP || idp == githubIDP:
		return fmt.Errorf("%w: %q is not an outside identity provider", ErrInvalid, idp)
	}
	return nil
}

func addTokens(byToken map[string]tokenOwner, r record) {
	if r.Disabled {
		return
	}
	if r.tokenSHA256 != "" {
		byToken[r.tokenSHA256] = tokenOwner{user: r.ID}
	}
	for _, d := range r.devices {
		byToken[d.sha256] = tokenOwner{user: r.ID, device: d.id}
	}
}

func userOf(payload string) (string, bool) {
	if id, ok := strings.CutPrefix(payload, userPayload); ok {
		return id, true
	}
	return strings.CutPrefix(payload, createdPayload)
}

func dropTokens(byToken map[string]tokenOwner, r record) {
	delete(byToken, r.tokenSHA256)
	for _, d := range r.devices {
		delete(byToken, d.sha256)
	}
}

// liveIn returns key's context in lives, made under parent when it has none or its last one ended with the parent.
func liveIn(parent context.Context, lives map[string]life, key string) context.Context {
	l, ok := lives[key]
	if !ok || l.ctx.Err() != nil {
		l.ctx, l.cancel = context.WithCancel(parent)
		lives[key] = l
	}
	return l.ctx
}

func hashPassword(password string, cost int) ([]byte, error) {
	if len(password) < minPassword || len(password) > maxPassword {
		return nil, fmt.Errorf("%w: a password is %d to %d bytes", ErrInvalid, minPassword, maxPassword)
	}
	return bcrypt.GenerateFromPassword([]byte(password), cost)
}

func deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
