// Package users keeps cocoonbox users in PostgreSQL, each with one gw key, one sandboxd tenant and one state key that never leave the api, their boxes and the boxes' shares.
package users

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/crypto/bcrypt"

	"github.com/loomhand/cocoonbox/api/gwkeys"
	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	// TokenPrefix starts every API token and device token.
	TokenPrefix = "cb_"

	idPrefix     = "u-"
	idBytes      = 6
	secretBytes  = 32
	sessionTTL   = 12 * time.Hour
	minPassword  = 8
	maxPassword  = 72
	maxBoxesCap  = 16
	defaultBoxes = 1

	tokenMissInterval = 100 * time.Millisecond
	listenRetry       = 2 * time.Second
	endedGrace        = 10 * time.Minute

	columnGWKey    = "gw_key"
	columnTenant   = "tenant_token"
	columnStateKey = "state_key"
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

// User is a cocoonbox user as the console shows it; it never carries a credential. MaxRunning 0 is the api's default.
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
}

// NewUser describes a user to create; an empty ID draws a fresh one, an empty Password leaves password sign-in off, an empty IDP
// records password, and a nil DailyCapMicros keeps the gw tenant default.
type NewUser struct {
	ID             string
	Name           string
	Password       string
	Admin          bool
	IDP            string
	MaxAgentBoxes  *int
	DailyCapMicros *int64
}

// Settings changes a user's settings; a nil field keeps its value, MaxRunning 0 returns the user to the api's default and
// MaxAgentBoxes 0 drops the user's agent box cap. Admin gives the user no gw cap, or the tenant's default when it ends.
type Settings struct {
	Admin         *bool `json:"admin,omitempty"`
	Disabled      *bool `json:"disabled,omitempty"`
	Residential   *bool `json:"residential,omitempty"`
	MaxBoxes      *int  `json:"max_boxes,omitempty"`
	MaxBoxesCap   *int  `json:"max_boxes_cap,omitempty"`
	MaxRunning    *int  `json:"max_running,omitempty"`
	MaxAgentBoxes *int  `json:"max_agent_boxes,omitempty"`
}

// GWKeys issues, revokes and meters each user's gw key and keeps the user's own daily cap, found by owner = user id.
type GWKeys interface {
	Add(ctx context.Context, owner string) (string, string, error)
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
}

// Service owns the users, boxes and shares tables and an in-memory index that serves every token, box and share check.
type Service struct {
	store        *store
	seal         *sealer
	gw           GWKeys
	tenants      machines.Tenants
	tenantClaims bool
	firstBoxID   func(string) string
	nextMiss     atomic.Int64
	// dummyHash keeps a sign-in for an unknown name as slow as one for a known name.
	dummyHash []byte

	mu           sync.RWMutex
	byID         map[string]record
	byToken      map[string]tokenOwner
	deviceUse    sync.Map
	boxes        map[string][]Box
	boxGen       int
	shares       map[string]*shareRecord
	shareSecrets map[string]string
	// shareGen counts this api's share changes, so a sync read before one does not undo it.
	shareGen      int
	onResidential []func(context.Context, string, bool)
	onShareEnd    []func(context.Context, Share)

	lifeMu sync.Mutex
	lives  map[string]life
	creds  map[string]*credLife
	tombs  map[string]time.Time
}

// Open connects to the database, applies migrations and loads every user.
func Open(ctx context.Context, opts Options) (*Service, error) {
	seal, err := newSealer(opts.MasterKeyFile)
	if err != nil {
		return nil, err
	}
	st, err := openStore(ctx, opts.DSN)
	if err != nil {
		return nil, err
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte(randomHex(secretBytes)), bcrypt.DefaultCost)
	if err != nil {
		st.close()
		return nil, err
	}
	s := &Service{
		store: st, seal: seal, gw: opts.GW, tenants: opts.Tenants, tenantClaims: opts.TenantClaims, firstBoxID: opts.FirstBoxID, dummyHash: dummy,
		boxes: map[string][]Box{}, shares: map[string]*shareRecord{}, shareSecrets: map[string]string{}, lives: map[string]life{},
		creds: map[string]*credLife{}, tombs: map[string]time.Time{},
	}
	if err := s.Load(ctx); err != nil {
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

// Load replaces the in-memory index with the tables; the boxes stay when a box changed meanwhile.
func (s *Service) Load(ctx context.Context) error {
	s.mu.RLock()
	gen := s.boxGen
	s.mu.RUnlock()
	records, err := s.store.all(ctx)
	if err != nil {
		return err
	}
	devices, err := s.store.allDevices(ctx)
	if err != nil {
		return err
	}
	boxes, err := s.store.allBoxes(ctx)
	if err != nil {
		return err
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID, s.byToken = byID, byToken
	if s.boxGen == gen {
		s.boxes = byOwner
	}
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
	return nil
}

// Listen reloads the index on every Notify until ctx ends. It reloads after each LISTEN as well, since a notification
// sent while no connection listens is lost; a failed reload or connection listens again after listenRetry.
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

// Notify tells every api that listens to reload its index; the CLI calls it after it writes users or tokens.
func (s *Service) Notify(ctx context.Context) error {
	return s.store.notify(ctx)
}

// Reconcile writes every user's tenant with its token and cap, so sandboxd matches the table, turns off disabled users' gw keys and lifts the gw cap of admins on the default.
func (s *Service) Reconcile(ctx context.Context) error {
	var errs []error
	for _, r := range s.records() {
		token, err := s.seal.open(r.tenantToken, userData(r.ID, columnTenant))
		if err == nil {
			err = s.tenants.PutTenant(ctx, r.ID, token, s.claimCap(r))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant of %s: %w", r.Name, err))
		}
		if r.Disabled {
			if err = s.gw.SetEnabled(ctx, r.ID, false); err != nil {
				errs = append(errs, fmt.Errorf("gw key of %s: %w", r.Name, err))
			}
		}
		if !r.Admin {
			continue
		}
		daily, err := s.gw.DailyCap(ctx, r.ID)
		if err == nil && daily == nil {
			err = s.gw.SetDailyCap(ctx, r.ID, new(int64(gwkeys.Unlimited)))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("gw cap of %s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}

// ByTokenSHA256 returns the enabled user whose API token or device token hashes to sum; an index miss checks the tables
// at most once per tokenMissInterval.
func (s *Service) ByTokenSHA256(ctx context.Context, sum string) (string, bool) {
	owner, ok := s.indexedToken(sum)
	if !ok && s.allowMiss() {
		if serves, err := s.store.tokenServes(ctx, sum); err == nil && serves && s.Load(ctx) == nil {
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
	return r.User, err
}

// ByName returns the user signed in as name.
func (s *Service) ByName(name string) (User, error) {
	for _, r := range s.records() {
		if r.Name == name {
			return r.User, nil
		}
	}
	return User{}, ErrNotFound
}

// List returns every user ordered by name.
func (s *Service) List() []User {
	records := s.records()
	out := make([]User, 0, len(records))
	for _, r := range records {
		out = append(out, r.User)
	}
	return out
}

// Create adds a user with its first box, gw key and tenant and lifts an admin's gw cap; a failure leaves none of them behind.
func (s *Service) Create(ctx context.Context, nu NewUser) (User, error) {
	id := cmp.Or(nu.ID, idPrefix+randomHex(idBytes))
	switch {
	case !namePattern.MatchString(nu.Name):
		return User{}, fmt.Errorf("%w: a username is 2 to 32 lowercase letters, digits, - or _, starting with a letter", ErrInvalid)
	case !idPattern.MatchString(id):
		return User{}, fmt.Errorf("%w: id %q is not a sandboxd tenant name", ErrInvalid, id)
	case nu.MaxAgentBoxes != nil && (*nu.MaxAgentBoxes < 1 || *nu.MaxAgentBoxes > maxBoxesCap):
		return User{}, fmt.Errorf("%w: max agent boxes must be in 1..%d", ErrInvalid, maxBoxesCap)
	}
	var hash []byte
	if nu.Password != "" {
		var err error
		if hash, err = hashPassword(nu.Password); err != nil {
			return User{}, err
		}
	}
	if _, err := s.ByName(nu.Name); err == nil {
		return User{}, ErrExists
	}
	if _, err := s.record(id); err == nil {
		return User{}, ErrExists
	}
	tenantToken := randomHex(secretBytes)
	stateKey := keyring.Generate(id)
	key, akID, err := s.gw.Add(ctx, id)
	if err != nil {
		return User{}, fmt.Errorf("issue gw key: %w", err)
	}
	r := record{
		ID:            id,
		Name:          nu.Name,
		IDP:           cmp.Or(nu.IDP, "password"),
		Admin:         nu.Admin,
		MaxBoxes:      defaultBoxes,
		MaxBoxesCap:   defaultBoxes,
		MaxAgentBoxes: nu.MaxAgentBoxes,
		CreatedAt:     time.Now(),
		gwAKID:        akID,
		gwKey:         s.seal.seal(key, userData(id, columnGWKey)),
		tenantToken:   s.seal.seal(tenantToken, userData(id, columnTenant)),
		stateKeyID:    stateKey.ID,
		stateKey:      s.seal.seal(string(stateKey.Secret), userData(id, columnStateKey)),
	}
	first := Box{ID: s.firstBoxID(id), Owner: id, Name: FirstBoxName, CreatedAt: r.CreatedAt.Truncate(time.Microsecond)}
	switch {
	case nu.Admin:
		err = s.gw.SetDailyCap(ctx, id, new(int64(gwkeys.Unlimited)))
	case nu.DailyCapMicros != nil:
		err = s.gw.SetDailyCap(ctx, id, nu.DailyCapMicros)
	}
	if err == nil {
		if err = s.tenants.PutTenant(ctx, id, tenantToken, s.claimCap(r)); err == nil {
			if err = s.store.insert(ctx, r, string(hash), first); err != nil {
				err = errors.Join(err, s.tenants.DeleteTenant(context.WithoutCancel(ctx), id))
			}
		}
	}
	if err != nil {
		return User{}, errors.Join(err, s.gw.SetDailyCap(context.WithoutCancel(ctx), id, nil), s.gw.Revoke(context.WithoutCancel(ctx), id))
	}
	s.published(ctx, "created user "+r.Name)
	s.put(r)
	s.mu.Lock()
	s.putBox(first)
	s.mu.Unlock()
	return r.User, nil
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
	s.published(ctx, "deleted user "+id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.byID[id]; ok {
		dropTokens(s.byToken, r)
		delete(s.byID, id)
	}
	s.boxGen++
	delete(s.boxes, id)
	s.cutOwner(id, true)
	s.end(id)
	return nil
}

// Update applies set to user id; the tenant's cap follows MaxBoxes, and a disabled user's gw key and open streams stop.
func (s *Service) Update(ctx context.Context, id string, set Settings) (User, error) {
	r, err := s.record(id)
	if err != nil {
		return User{}, err
	}
	old := r
	if set.MaxBoxesCap != nil {
		r.MaxBoxesCap = *set.MaxBoxesCap
		r.MaxBoxes = min(r.MaxBoxes, r.MaxBoxesCap)
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
	switch {
	case r.MaxBoxesCap < 1 || r.MaxBoxesCap > maxBoxesCap:
		return User{}, fmt.Errorf("%w: max boxes cap must be in 1..%d", ErrInvalid, maxBoxesCap)
	case r.MaxBoxes < 1 || r.MaxBoxes > r.MaxBoxesCap:
		return User{}, fmt.Errorf("%w: max boxes must be in 1..%d", ErrInvalid, r.MaxBoxesCap)
	case r.MaxBoxes < old.MaxBoxes && r.MaxBoxes < len(s.Boxes(id))-s.agentBoxes(id):
		return User{}, fmt.Errorf("%w: the user has %d boxes; delete boxes before lowering max boxes", ErrInvalid, len(s.Boxes(id))-s.agentBoxes(id))
	case r.MaxRunning < 0 || r.MaxRunning > r.MaxBoxes:
		return User{}, fmt.Errorf("%w: the running limit must be in 1..%d (max boxes), or 0 for the default; lower it before max boxes", ErrInvalid, r.MaxBoxes)
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
	if err := s.store.update(ctx, r); err != nil {
		return User{}, err
	}
	if r.Disabled && !old.Disabled {
		if err := s.store.removeSessions(ctx, id); err != nil {
			return User{}, err
		}
	}
	s.published(ctx, "updated user "+id)
	s.put(r)
	if r.Disabled && !old.Disabled {
		s.mu.Lock()
		s.cutOwner(id, false)
		s.end(id)
		s.mu.Unlock()
	}
	if r.Residential != old.Residential {
		s.mu.RLock()
		hooks := slices.Clone(s.onResidential)
		s.mu.RUnlock()
		for _, fn := range hooks {
			fn(ctx, id, r.Residential)
		}
	}
	return r.User, nil
}

// SetPassword replaces user id's password and ends its console sessions.
func (s *Service) SetPassword(ctx context.Context, id, password string) error {
	if _, err := s.record(id); err != nil {
		return err
	}
	hash, err := hashPassword(password)
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
	old := r.tokenSHA256
	r.tokenSHA256, r.HasToken = sum, true
	s.put(r)
	if old != sum {
		s.EndCredential(ctx, old)
	}
	s.published(ctx, "set the token of "+id)
	return nil
}

// RegenerateToken gives user id a new API token, returned once; the old one stops working at once.
func (s *Service) RegenerateToken(ctx context.Context, id string) (string, error) {
	token := TokenPrefix + randomHex(secretBytes)
	if err := s.SetTokenSHA256(ctx, id, hashHex(token)); err != nil {
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
	old := r.tokenSHA256
	r.tokenSHA256, r.HasToken = "", false
	s.put(r)
	s.EndCredential(ctx, old)
	s.published(ctx, "revoked the token of "+id)
	return nil
}

// SignOut ends every sign-in of user id at once: its console sessions, personal token and device tokens, and every call open on them.
func (s *Service) SignOut(ctx context.Context, id string) error {
	if _, err := s.record(id); err != nil {
		return err
	}
	if err := s.store.signOut(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
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
	s.mu.Unlock()
	s.published(ctx, "signed "+id+" out everywhere")
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
	if err := s.Load(ctx); err != nil {
		return User{}, err
	}
	return s.Get(id)
}

// NewSession starts a console session for user id and returns its secret.
func (s *Service) NewSession(ctx context.Context, id string) (string, error) {
	secret := randomHex(secretBytes)
	if err := s.store.addSession(ctx, hashHex(secret), id, time.Now().Add(sessionTTL)); err != nil {
		return "", err
	}
	return secret, nil
}

// Session returns the enabled user of the session with secret.
func (s *Service) Session(ctx context.Context, secret string) (User, error) {
	id, err := s.SessionUser(ctx, hashHex(secret))
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
	return s.store.removeSession(ctx, hashHex(secret))
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

// GWKey returns user id's gw key.
func (s *Service) GWKey(id string) (string, error) {
	r, err := s.record(id)
	if err != nil {
		return "", err
	}
	return s.seal.open(r.gwKey, userData(id, columnGWKey))
}

// StateKey returns user id's state key.
func (s *Service) StateKey(_ context.Context, id string) (keyring.Key, error) {
	r, err := s.record(id)
	if err != nil {
		return keyring.Key{}, err
	}
	if r.stateKeyID == "" {
		return keyring.Key{}, fmt.Errorf("user %s has no state key", id)
	}
	secret, err := s.seal.open(r.stateKey, userData(id, columnStateKey))
	if err != nil {
		return keyring.Key{}, err
	}
	return keyring.Key{ID: r.stateKeyID, Secret: []byte(secret)}, nil
}

// SetStateKey gives user id the state key k; it is a no-op when the user holds k and an error when it holds another key.
func (s *Service) SetStateKey(ctx context.Context, id string, k keyring.Key) error {
	if len(k.Secret) != keyring.KeySize || k.ID == "" {
		return fmt.Errorf("%w: a state key has an id and %d bytes", ErrInvalid, keyring.KeySize)
	}
	r, err := s.record(id)
	if err != nil {
		return err
	}
	if r.stateKeyID != "" {
		held, err := s.StateKey(ctx, id)
		if err != nil {
			return err
		}
		if held.ID != k.ID || !bytes.Equal(held.Secret, k.Secret) {
			return fmt.Errorf("%w: user %s holds state key %s", ErrExists, id, held.ID)
		}
		return nil
	}
	r.stateKeyID, r.stateKey = k.ID, s.seal.seal(string(k.Secret), userData(id, columnStateKey))
	if err := s.store.setStateKey(ctx, r); err != nil {
		return err
	}
	s.published(ctx, "set the state key of "+id)
	s.put(r)
	return nil
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

// EndCredential ends the calls bound to credential, a token's sha256 or a grant id, in every api; an ended grant id stays ended.
func (s *Service) EndCredential(ctx context.Context, credential string) {
	s.endLocally(credential)
	if credential == "" {
		return
	}
	if err := s.store.notifyEnded(ctx, credential); err != nil {
		log.WithFunc("users.Service.EndCredential").Warnf(ctx, "other apis end the calls of an ended credential at their next reload only: %v", err)
	}
}

// OnResidential registers fn to run after a user's residential setting changes.
func (s *Service) OnResidential(fn func(ctx context.Context, id string, on bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onResidential = append(s.onResidential, fn)
}

// endLocally ends the calls bound to credential in this api, for an ended credential of any api.
func (s *Service) endLocally(credential string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endCredential(credential)
	if credential == "" || hexSHA256.MatchString(credential) {
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
			if err := s.Load(ctx); err != nil {
				return err
			}
			if err := s.syncShares(ctx); err != nil {
				return err
			}
		}
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		reload = n.Channel != endedChannel
		if !reload {
			s.endLocally(n.Payload)
		}
	}
}

// published tells every api to reload after a write; a failed notify reaches them with the next one.
func (s *Service) published(ctx context.Context, what string) {
	if err := s.store.notify(ctx); err != nil {
		log.WithFunc("users.Service.published").Warnf(ctx, "%s; other apis reload on their next notify: %v", what, err)
	}
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

// put stores r in the index in place of its previous version; the device tokens stay those of the index.
func (s *Service) put(r record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byID[r.ID]; ok {
		r.devices = old.devices
		dropTokens(s.byToken, old)
	}
	s.byID[r.ID] = r
	addTokens(s.byToken, r)
}

// record is a user row with the values the console never sees.
type record struct {
	User
	tokenSHA256 string
	devices     []deviceToken
	gwAKID      string
	gwKey       []byte
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

func hashPassword(password string) ([]byte, error) {
	if len(password) < minPassword || len(password) > maxPassword {
		return nil, fmt.Errorf("%w: a password is %d to %d bytes", ErrInvalid, minPassword, maxPassword)
	}
	return bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
}

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func hashHex(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
