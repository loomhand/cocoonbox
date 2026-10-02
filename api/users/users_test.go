package users

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/CMGS/cocoonbox/api/gwkeys"
	"github.com/CMGS/cocoonbox/api/keyring"
)

const password = "correct horse battery"

func TestCreateIssuesBothKeysAndSealsThem(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u, err := svc.Create(t.Context(), NewUser{Name: "qa1", Password: password})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(u.ID, "u-") || u.MaxBoxes != 1 || u.MaxBoxesCap != 1 || u.HasToken || u.Admin {
		t.Errorf("user = %+v", u)
	}
	key, err := svc.GWKey(u.ID)
	if err != nil || key != gw.keys[u.ID] {
		t.Errorf("GWKey = %q, %v, want the key gw issued for owner %s", key, err, u.ID)
	}
	token, err := svc.Tenant(u.ID)
	if err != nil || token == "" || tenants.tenants[u.ID] != (tenant{token: token, max: 2}) {
		t.Errorf("Tenant = %q, %v; sandboxd holds %+v", token, err, tenants.tenants)
	}
	var sealedKey, sealedToken []byte
	if err := query(t, dsn, "SELECT gw_key_sealed, tenant_token_sealed FROM users WHERE id = $1", u.ID).Scan(&sealedKey, &sealedToken); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if strings.Contains(string(sealedKey), key) || strings.Contains(string(sealedToken), token) {
		t.Error("the database holds a backing key in the clear")
	}
	if _, err := svc.Create(t.Context(), NewUser{Name: "qa1", Password: password}); !errors.Is(err, ErrExists) {
		t.Errorf("Create of a taken name: %v, want ErrExists", err)
	}
	reopened := openTestService(t, dsn, gw, tenants)
	if again, err := reopened.GWKey(u.ID); err != nil || again != key {
		t.Errorf("GWKey after reopen = %q, %v", again, err)
	}
	reopened.tenantClaims = false
	if token, err := reopened.Tenant(u.ID); err != nil || token != "" {
		t.Errorf("Tenant with root claims = %q, %v, want empty", token, err)
	}
}

func TestCreateLeavesNothingBehindOnFailure(t *testing.T) {
	svc, gw, tenants, _ := newTestService(t)
	tenants.err = errors.New("sandboxd 503")
	if _, err := svc.Create(t.Context(), NewUser{Name: "qa1"}); err == nil {
		t.Fatal("Create succeeded with sandboxd down")
	}
	if len(gw.keys) != 0 || len(svc.List()) != 0 {
		t.Errorf("after a failed Create: gw keys %v, users %v", gw.keys, svc.List())
	}
	for _, nu := range []NewUser{{Name: "Q"}, {Name: "qa1", Password: "short"}, {Name: "qa1", ID: "bad/id"}} {
		if _, err := svc.Create(t.Context(), nu); !errors.Is(err, ErrInvalid) {
			t.Errorf("Create(%+v) = %v, want ErrInvalid", nu, err)
		}
	}
}

func TestTokensPasswordsAndDisabling(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	u, err := svc.Create(t.Context(), NewUser{Name: "qa1", Password: password})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := svc.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	if id, ok := svc.ByTokenSHA256(sum(first)); !ok || id != u.ID {
		t.Fatalf("token resolves to %q, %t", id, ok)
	}
	second, _ := svc.RegenerateToken(t.Context(), u.ID)
	if _, ok := svc.ByTokenSHA256(sum(first)); ok {
		t.Error("the old token still works after regenerate")
	}
	if _, err := svc.Login(t.Context(), "qa1", password); err != nil {
		t.Errorf("Login: %v", err)
	}
	for name, pw := range map[string]string{"qa1": "wrong password!", "nobody": password} {
		if _, err := svc.Login(t.Context(), name, pw); !errors.Is(err, ErrBadPassword) {
			t.Errorf("Login(%s) = %v, want ErrBadPassword", name, err)
		}
	}
	secret, err := svc.NewSession(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if got, err := svc.Session(t.Context(), secret); err != nil || got.ID != u.ID {
		t.Errorf("Session = %+v, %v", got, err)
	}
	disabled := true
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: &disabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok := svc.ByTokenSHA256(sum(second)); ok {
		t.Error("a disabled user's token still works")
	}
	if _, err := svc.Session(t.Context(), secret); !errors.Is(err, ErrNotFound) {
		t.Errorf("Session of a disabled user = %v", err)
	}
	if _, err := svc.Login(t.Context(), "qa1", password); !errors.Is(err, ErrBadPassword) {
		t.Errorf("Login of a disabled user = %v", err)
	}
	other, _ := svc.Create(t.Context(), NewUser{Name: "qa2"})
	if err := svc.SetTokenSHA256(t.Context(), other.ID, sum(second)); !errors.Is(err, ErrExists) {
		t.Errorf("SetTokenSHA256 with another user's hash = %v, want ErrExists", err)
	}
}

func TestUpdateKeepsBoxesUnderTheCapAndMovesTheTenant(t *testing.T) {
	svc, _, tenants, _ := newTestService(t)
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	var flips []bool
	svc.OnResidential(func(_ context.Context, id string, on bool) {
		if id == u.ID {
			flips = append(flips, on)
		}
	})
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxes: new(2)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("max boxes over the cap = %v, want ErrInvalid", err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(3)}); err != nil {
		t.Fatalf("raise cap: %v", err)
	}
	if got, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxes: new(2), Residential: new(true)}); err != nil || got.MaxBoxes != 2 || !got.Residential {
		t.Fatalf("Update = %+v, %v", got, err)
	}
	if tenants.tenants[u.ID].max != 3 || !svc.Residential(u.ID) {
		t.Errorf("tenant cap %d, residential %t", tenants.tenants[u.ID].max, svc.Residential(u.ID))
	}
	if got, _ := svc.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(1)}); got.MaxBoxes != 1 || tenants.tenants[u.ID].max != 2 {
		t.Errorf("lowering the cap left max boxes %d, tenant cap %d", got.MaxBoxes, tenants.tenants[u.ID].max)
	}
	if svc.Residential("u-unknown") || len(flips) != 1 || !flips[0] {
		t.Errorf("residential flips = %v", flips)
	}
}

func TestDeleteRemovesTenantKeyRowAndSessions(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1", Password: password})
	token, _ := svc.RegenerateToken(t.Context(), u.ID)
	if _, err := svc.NewSession(t.Context(), u.ID); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := svc.Delete(t.Context(), u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := gw.keys[u.ID]; ok {
		t.Error("gw key kept")
	}
	if _, ok := tenants.tenants[u.ID]; ok {
		t.Error("tenant kept")
	}
	if _, ok := svc.ByTokenSHA256(sum(token)); ok {
		t.Error("the deleted user's token still works")
	}
	var sessions int
	if err := query(t, dsn, "SELECT count(*) FROM sessions").Scan(&sessions); err != nil || sessions != 0 {
		t.Errorf("sessions left = %d, %v", sessions, err)
	}
	if err := svc.Delete(t.Context(), u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestASealedValueOpensOnlyOnItsOwnRow(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	a, _ := svc.Create(t.Context(), NewUser{Name: "alice"})
	b, _ := svc.Create(t.Context(), NewUser{Name: "bob"})
	if err := exec(t, dsn, "UPDATE users SET gw_key_sealed = (SELECT gw_key_sealed FROM users WHERE id = $1) WHERE id = $2", a.ID, b.ID); err != nil {
		t.Fatalf("copy sealed key: %v", err)
	}
	reopened := openTestService(t, dsn, gw, tenants)
	if _, err := reopened.GWKey(b.ID); err == nil {
		t.Error("alice's sealed gw key opened as bob's")
	}
	if _, err := reopened.Tenant(b.ID); err != nil {
		t.Errorf("bob's own tenant token: %v", err)
	}
}

func TestStateKeyIsSealedAndSetOnlyOnce(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	created, err := svc.StateKey(t.Context(), u.ID)
	if err != nil || len(created.Secret) != keyring.KeySize || created.ID == "" {
		t.Fatalf("StateKey of a new user = %d bytes, id %q, %v", len(created.Secret), created.ID, err)
	}
	var sealed []byte
	if err := query(t, dsn, "SELECT state_key_sealed FROM users WHERE id = $1", u.ID).Scan(&sealed); err != nil || bytes.Contains(sealed, created.Secret) {
		t.Errorf("state key column holds the key in the clear or is unreadable: %v", err)
	}

	if err := exec(t, dsn, "UPDATE users SET state_key_id = NULL, state_key_sealed = NULL WHERE id = $1", u.ID); err != nil {
		t.Fatalf("clear state key: %v", err)
	}
	migrated := openTestService(t, dsn, gw, tenants)
	if _, err := migrated.StateKey(t.Context(), u.ID); err == nil {
		t.Error("StateKey of a user without one succeeded")
	}
	boxKey := keyring.Key{ID: "file:cb-000000000000:1", Secret: bytes.Repeat([]byte{7}, keyring.KeySize)}
	for range 2 {
		if err := migrated.SetStateKey(t.Context(), u.ID, boxKey); err != nil {
			t.Fatalf("SetStateKey: %v", err)
		}
	}
	if err := migrated.SetStateKey(t.Context(), u.ID, keyring.Generate(u.ID)); !errors.Is(err, ErrExists) {
		t.Errorf("SetStateKey over another key = %v, want ErrExists", err)
	}
	if err := migrated.SetStateKey(t.Context(), u.ID, keyring.Key{ID: "short", Secret: []byte{1}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetStateKey of a short key = %v, want ErrInvalid", err)
	}
	reopened := openTestService(t, dsn, gw, tenants)
	if got, err := reopened.StateKey(t.Context(), u.ID); err != nil || got.ID != boxKey.ID || !bytes.Equal(got.Secret, boxKey.Secret) {
		t.Errorf("StateKey after reopen = %q, %v; want the box key", got.ID, err)
	}

	other, _ := reopened.Create(t.Context(), NewUser{Name: "qa2"})
	if err := exec(t, dsn, "UPDATE users SET state_key_sealed = (SELECT state_key_sealed FROM users WHERE id = $1) WHERE id = $2", u.ID, other.ID); err != nil {
		t.Fatalf("copy sealed state key: %v", err)
	}
	if _, err := openTestService(t, dsn, gw, tenants).StateKey(t.Context(), other.ID); err == nil {
		t.Error("qa1's sealed state key opened as qa2's")
	}
}

func TestAdminsHaveNoGWCapAndDeleteDropsTheCap(t *testing.T) {
	svc, gw, _, _ := newTestService(t)
	admin, err := svc.Create(t.Context(), NewUser{Name: "root", Admin: true})
	if err != nil {
		t.Fatalf("Create admin: %v", err)
	}
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	if caps, _ := svc.DailyCaps(t.Context()); fmt.Sprint(caps) != fmt.Sprint(map[string]int64{admin.ID: gwkeys.Unlimited}) {
		t.Errorf("gw caps after create = %v, want only the admin unlimited", caps)
	}
	for _, bad := range []int64{-2, -100} {
		if err := svc.SetDailyCap(t.Context(), u.ID, &bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("SetDailyCap(%d) = %v, want ErrInvalid", bad, err)
		}
	}
	if err := svc.SetDailyCap(t.Context(), "u-unknown", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDailyCap of an unknown user = %v, want ErrNotFound", err)
	}
	if err := svc.SetDailyCap(t.Context(), u.ID, new(int64(100))); err != nil {
		t.Fatalf("SetDailyCap: %v", err)
	}
	if got, err := svc.DailyCap(t.Context(), u.ID); err != nil || got == nil || *got != 100 {
		t.Errorf("DailyCap = %v, %v, want 100", got, err)
	}
	if err := svc.Delete(t.Context(), u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := gw.caps[u.ID]; ok {
		t.Error("the deleted user's gw cap stayed")
	}
}

func TestReconcileLiftsTheCapOfAdminsOnTheDefaultOnly(t *testing.T) {
	svc, gw, _, _ := newTestService(t)
	onDefault, _ := svc.Create(t.Context(), NewUser{Name: "root", Admin: true})
	capped, _ := svc.Create(t.Context(), NewUser{Name: "ops", Admin: true})
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	_ = svc.SetDailyCap(t.Context(), onDefault.ID, nil)
	_ = svc.SetDailyCap(t.Context(), capped.ID, new(int64(7)))
	if err := svc.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := map[string]int64{onDefault.ID: gwkeys.Unlimited, capped.ID: 7}
	if fmt.Sprint(gw.caps) != fmt.Sprint(want) {
		t.Errorf("gw caps after Reconcile = %v, want %v and none for %s", gw.caps, want, u.ID)
	}
}

type fakeGW struct {
	mu   sync.Mutex
	keys map[string]string
	caps map[string]int64
}

func (g *fakeGW) Add(_ context.Context, owner string) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.keys[owner] = "gw-" + randomHex(8)
	return g.keys[owner], "sha256:" + owner, nil
}

func (g *fakeGW) Revoke(_ context.Context, owner string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.keys, owner)
	return nil
}

func (g *fakeGW) Usage(context.Context, string, time.Time) ([]gwkeys.Usage, error) {
	return nil, nil
}

func (g *fakeGW) DailyCap(_ context.Context, owner string) (*int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if micros, ok := g.caps[owner]; ok {
		return &micros, nil
	}
	return nil, nil
}

func (g *fakeGW) DailyCaps(context.Context) (map[string]int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.caps), nil
}

func (g *fakeGW) SetDailyCap(_ context.Context, owner string, micros *int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if micros == nil {
		delete(g.caps, owner)
	} else {
		g.caps[owner] = *micros
	}
	return nil
}

type tenant struct {
	token string
	max   int
}

type fakeTenants struct {
	mu      sync.Mutex
	tenants map[string]tenant
	err     error
}

func (f *fakeTenants) Put(_ context.Context, name, token string, maxClaims int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.tenants[name] = tenant{token: cmp.Or(token, f.tenants[name].token), max: maxClaims}
	return nil
}

func (f *fakeTenants) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tenants, name)
	return nil
}

// newTestService opens a Service on a fresh database of the server COCOONBOX_TEST_DSN names.
func newTestService(t *testing.T) (*Service, *fakeGW, *fakeTenants, string) {
	t.Helper()
	dsn := freshDatabase(t)
	gw := &fakeGW{keys: map[string]string{}, caps: map[string]int64{}}
	tenants := &fakeTenants{tenants: map[string]tenant{}}
	return openTestService(t, dsn, gw, tenants), gw, tenants, dsn
}

func openTestService(t *testing.T, dsn string, gw *fakeGW, tenants *fakeTenants) *Service {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("k", masterKeySize)), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	svc, err := Open(t.Context(), Options{DSN: dsn, MasterKeyFile: keyFile, GW: gw, Tenants: tenants, TenantClaims: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

func freshDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("COCOONBOX_TEST_DSN")
	if base == "" {
		t.Skip("COCOONBOX_TEST_DSN names no PostgreSQL server")
	}
	name := "t_" + randomHex(6)
	if err := exec(t, base, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _ = exec(t, base, "DROP DATABASE "+name+" WITH (FORCE)") })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse COCOONBOX_TEST_DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func query(t *testing.T, dsn, sql string, args ...any) pgx.Row {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn.QueryRow(t.Context(), sql, args...)
}

func exec(t *testing.T, dsn, sql string, args ...any) error {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	_, err = conn.Exec(context.Background(), sql, args...)
	return err
}

func sum(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}
