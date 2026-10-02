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
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/crypto/bcrypt"

	"github.com/CMGS/cocoonbox/api/gwkeys"
	"github.com/CMGS/cocoonbox/api/keyring"
)

const (
	idPrefix     = "u-"
	idBytes      = 6
	tokenPrefix  = "cb_"
	secretBytes  = 32
	sessionTTL   = 12 * time.Hour
	minPassword  = 8
	maxPassword  = 72
	maxBoxesCap  = 16
	defaultBoxes = 1

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

// User is a cocoonbox user as the console shows it; it never carries a credential.
type User struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	IDP         string    `json:"idp"`
	Admin       bool      `json:"admin"`
	Disabled    bool      `json:"disabled"`
	Residential bool      `json:"residential"`
	MaxBoxes    int       `json:"max_boxes"`
	MaxBoxesCap int       `json:"max_boxes_cap"`
	HasToken    bool      `json:"has_token"`
	CreatedAt   time.Time `json:"created_at"`
}

// NewUser describes a user to create; an empty ID draws a fresh one and an empty Password leaves sign-in off.
type NewUser struct {
	ID       string
	Name     string
	Password string
	Admin    bool
}

// Settings changes a user's settings; a nil field keeps its value.
type Settings struct {
	Disabled    *bool `json:"disabled,omitempty"`
	Residential *bool `json:"residential,omitempty"`
	MaxBoxes    *int  `json:"max_boxes,omitempty"`
	MaxBoxesCap *int  `json:"max_boxes_cap,omitempty"`
}

// GWKeys issues, revokes and meters each user's gw key and keeps the user's own daily cap, found by owner = user id.
type GWKeys interface {
	Add(ctx context.Context, owner string) (string, string, error)
	Revoke(ctx context.Context, owner string) error
	Usage(ctx context.Context, owner string, since time.Time) ([]gwkeys.Usage, error)
	DailyCap(ctx context.Context, owner string) (*int64, error)
	DailyCaps(ctx context.Context) (map[string]int64, error)
	SetDailyCap(ctx context.Context, owner string, micros *int64) error
}

// Tenants manages each user's sandboxd tenant, named by the user id.
type Tenants interface {
	Put(ctx context.Context, name, token string, maxClaims int) error
	Delete(ctx context.Context, name string) error
}

// Options names the database, the master key file and the backends that hold each user's keys.
type Options struct {
	DSN           string
	MasterKeyFile string
	GW            GWKeys
	Tenants       Tenants
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
	tenants      Tenants
	tenantClaims bool
	firstBoxID   func(string) string
	// dummyHash keeps a sign-in for an unknown name as slow as one for a known name.
	dummyHash []byte

	mu            sync.RWMutex
	byID          map[string]record
	byToken       map[string]string
	boxes         map[string][]Box
	boxGen        int
	shares        map[string]*shareRecord
	shareSecrets  map[string]string
	onResidential []func(context.Context, string, bool)
	onShareEnd    []func(context.Context, Share)
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
		boxes: map[string][]Box{}, shares: map[string]*shareRecord{}, shareSecrets: map[string]string{},
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
	boxes, err := s.store.allBoxes(ctx)
	if err != nil {
		return err
	}
	byOwner := map[string][]Box{}
	for _, b := range boxes {
		byOwner[b.Owner] = append(byOwner[b.Owner], b)
	}
	byID := make(map[string]record, len(records))
	byToken := make(map[string]string, len(records))
	for _, r := range records {
		byID[r.ID] = r
		if r.serves() {
			byToken[r.tokenSHA256] = r.ID
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID, s.byToken = byID, byToken
	if s.boxGen == gen {
		s.boxes = byOwner
	}
	return nil
}

// Run reloads the index every interval until ctx ends, which picks up changes made by the CLI.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := s.Load(ctx); err != nil {
			log.WithFunc("users.Service.Run").Warnf(ctx, "reload users: %v", err)
		}
	}
}

// Reconcile writes every user's tenant with its token and cap, so sandboxd matches the table, and lifts the gw cap of admins on the default.
func (s *Service) Reconcile(ctx context.Context) error {
	var errs []error
	for _, r := range s.records() {
		token, err := s.seal.open(r.tenantToken, r.ID, columnTenant)
		if err == nil {
			err = s.tenants.Put(ctx, r.ID, token, r.claimCap())
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant of %s: %w", r.Name, err))
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

// ByTokenSHA256 returns the enabled user whose API token hashes to sum.
func (s *Service) ByTokenSHA256(sum string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byToken[sum]
	return id, ok
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
		ID:          id,
		Name:        nu.Name,
		IDP:         "password",
		Admin:       nu.Admin,
		MaxBoxes:    defaultBoxes,
		MaxBoxesCap: defaultBoxes,
		CreatedAt:   time.Now(),
		gwAKID:      akID,
		gwKey:       s.seal.seal(key, id, columnGWKey),
		tenantToken: s.seal.seal(tenantToken, id, columnTenant),
		stateKeyID:  stateKey.ID,
		stateKey:    s.seal.seal(string(stateKey.Secret), id, columnStateKey),
	}
	first := Box{ID: s.firstBoxID(id), Owner: id, Name: FirstBoxName, CreatedAt: r.CreatedAt.Truncate(time.Microsecond)}
	if nu.Admin {
		err = s.gw.SetDailyCap(ctx, id, new(int64(gwkeys.Unlimited)))
	}
	if err == nil {
		if err = s.tenants.Put(ctx, id, tenantToken, r.claimCap()); err == nil {
			if err = s.store.insert(ctx, r, string(hash), first); err != nil {
				err = errors.Join(err, s.tenants.Delete(context.WithoutCancel(ctx), id))
			}
		}
	}
	if err != nil {
		return User{}, errors.Join(err, s.gw.SetDailyCap(context.WithoutCancel(ctx), id, nil), s.gw.Revoke(context.WithoutCancel(ctx), id))
	}
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
	if err := s.tenants.Delete(ctx, id); err != nil {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.byID[id]; ok {
		delete(s.byToken, r.tokenSHA256)
		delete(s.byID, id)
	}
	s.boxGen++
	delete(s.boxes, id)
	s.cutOwner(id, true)
	return nil
}

// Update applies set to user id; the tenant's cap follows MaxBoxes.
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
	r.Disabled = deref(set.Disabled, r.Disabled)
	r.Residential = deref(set.Residential, r.Residential)
	switch {
	case r.MaxBoxesCap < 1 || r.MaxBoxesCap > maxBoxesCap:
		return User{}, fmt.Errorf("%w: max boxes cap must be in 1..%d", ErrInvalid, maxBoxesCap)
	case r.MaxBoxes < 1 || r.MaxBoxes > r.MaxBoxesCap:
		return User{}, fmt.Errorf("%w: max boxes must be in 1..%d", ErrInvalid, r.MaxBoxesCap)
	case r.MaxBoxes < old.MaxBoxes && r.MaxBoxes < len(s.Boxes(id)):
		return User{}, fmt.Errorf("%w: the user has %d boxes; delete boxes before lowering max boxes", ErrInvalid, len(s.Boxes(id)))
	}
	if r.MaxBoxes != old.MaxBoxes {
		if err := s.tenants.Put(ctx, id, "", r.claimCap()); err != nil {
			return User{}, fmt.Errorf("set tenant cap: %w", err)
		}
	}
	if err := s.store.setSettings(ctx, r); err != nil {
		return User{}, err
	}
	if r.Disabled && !old.Disabled {
		if err := s.store.removeSessions(ctx, id); err != nil {
			return User{}, err
		}
	}
	s.put(r)
	if r.Disabled && !old.Disabled {
		s.mu.Lock()
		s.cutOwner(id, false)
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
	r.tokenSHA256, r.HasToken = sum, true
	s.put(r)
	return nil
}

// RegenerateToken gives user id a new API token, returned once; the old one stops working at once.
func (s *Service) RegenerateToken(ctx context.Context, id string) (string, error) {
	token := tokenPrefix + randomHex(secretBytes)
	sum := sha256.Sum256([]byte(token))
	if err := s.SetTokenSHA256(ctx, id, hex.EncodeToString(sum[:])); err != nil {
		return "", err
	}
	return token, nil
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
	id, err := s.store.session(ctx, hashHex(secret))
	if err != nil {
		return User{}, err
	}
	u, err := s.Get(id)
	if err == nil && u.Disabled {
		return User{}, ErrNotFound
	}
	return u, err
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
	return s.seal.open(r.tenantToken, id, columnTenant)
}

// GWKey returns user id's gw key.
func (s *Service) GWKey(id string) (string, error) {
	r, err := s.record(id)
	if err != nil {
		return "", err
	}
	return s.seal.open(r.gwKey, id, columnGWKey)
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
	secret, err := s.seal.open(r.stateKey, id, columnStateKey)
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
	r.stateKeyID, r.stateKey = k.ID, s.seal.seal(string(k.Secret), id, columnStateKey)
	if err := s.store.setStateKey(ctx, r); err != nil {
		return err
	}
	s.put(r)
	return nil
}

// Residential reports whether user id's boxes take the residential exit.
func (s *Service) Residential(id string) bool {
	r, err := s.record(id)
	return err == nil && r.Residential
}

// OnResidential registers fn to run after a user's residential setting changes.
func (s *Service) OnResidential(fn func(ctx context.Context, id string, on bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onResidential = append(s.onResidential, fn)
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

func (s *Service) records() []record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.SortedFunc(maps.Values(s.byID), func(a, b record) int { return strings.Compare(a.Name, b.Name) })
}

// put stores r in the index in place of its previous version.
func (s *Service) put(r record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byID[r.ID]; ok {
		delete(s.byToken, old.tokenSHA256)
	}
	s.byID[r.ID] = r
	if r.serves() {
		s.byToken[r.tokenSHA256] = r.ID
	}
}

// record is a user row with the values the console never sees.
type record struct {
	User
	tokenSHA256 string
	gwAKID      string
	gwKey       []byte
	tenantToken []byte
	stateKeyID  string
	stateKey    []byte
}

func (r record) serves() bool { return r.tokenSHA256 != "" && !r.Disabled }

// claimCap is the tenant's max_claims: the box quota plus the old machine a Rebase keeps until the new one serves.
func (r record) claimCap() int { return r.MaxBoxes + 1 }

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
