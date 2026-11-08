package users

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/gwkeys"
	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users/userstest"
	"github.com/loomhand/cocoonbox/api/utils"
)

const (
	password  = "correct horse battery"
	outsideID = "Fb7xQ2kLm9PzR4tVw1YcN8sJd3Hq"
)

var (
	oldFlavor = machines.Flavor{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40}
	newFlavor = machines.Flavor{VCPUs: 2, MemoryMiB: 4096, DiskGiB: 20}
)

func TestAnUpdateFromAStaleCacheKeepsTheColumnsItDoesNotSet(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	if err := userstest.Exec(t, dsn, "UPDATE users SET no_gc = true, image = 'cbcore:v1' WHERE id = $1", u.ID); err != nil {
		t.Fatalf("set the policy behind the cache: %v", err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxes: new(1)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if p := openTestService(t, dsn, gw, tenants).Policy(u.ID); p != (Policy{Image: "cbcore:v1", NoGC: true}) {
		t.Errorf("policy after an update from a stale cache = %+v, want the one the table held", p)
	}
}

func TestAUsersPolicyIsKeptAndReloaded(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	if p := svc.Policy(u.ID); p != (Policy{}) {
		t.Fatalf("a new user's policy = %+v, want the zero one", p)
	}
	want := Policy{Image: "cbcore:v1", Delta: true, NoPark: true, NoGC: true}
	if _, err := svc.Update(t.Context(), u.ID, Settings{Image: &want.Image, Delta: &want.Delta, NoPark: &want.NoPark, NoGC: &want.NoGC}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if p := svc.Policy(u.ID); p != want {
		t.Errorf("policy after Update = %+v, want %+v", p, want)
	}
	if p := openTestService(t, dsn, gw, tenants).Policy(u.ID); p != want {
		t.Errorf("policy another api loads = %+v, want %+v", p, want)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{NoPark: new(false)}); err != nil {
		t.Fatalf("Update of one field: %v", err)
	}
	want.NoPark = false
	if svc.Policy(u.ID) != want {
		t.Errorf("policy after an update of one field = %+v, want %+v", svc.Policy(u.ID), want)
	}
}

func TestCreateIssuesTheTenantTokenAndSealsIt(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1", Password: password})
	if !strings.HasPrefix(u.ID, "u-") || u.MaxBoxes != 1 || u.MaxBoxesCap != 1 || u.HasToken || u.Admin {
		t.Errorf("user = %+v", u)
	}
	token, err := svc.Tenant(u.ID)
	if err != nil || token == "" || tenants.Tokens[u.ID] != token || tenants.Max[u.ID] != 2 {
		t.Errorf("Tenant = %q, %v; sandboxd holds %v %v", token, err, tenants.Tokens, tenants.Max)
	}
	var sealedToken []byte
	if err := userstest.Query(t, dsn, "SELECT tenant_token_sealed FROM users WHERE id = $1", u.ID).Scan(&sealedToken); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if strings.Contains(string(sealedToken), token) {
		t.Error("the database holds a backing key in the clear")
	}
	if _, err := svc.Create(t.Context(), NewUser{Name: "qa1", Password: password}); !errors.Is(err, ErrExists) {
		t.Errorf("Create of a taken name: %v, want ErrExists", err)
	}
	reopened := openTestService(t, dsn, gw, tenants)
	if again, err := reopened.Tenant(u.ID); err != nil || again != token {
		t.Errorf("Tenant after reopen = %q, %v", again, err)
	}
	reopened.tenantClaims = false
	if token, err := reopened.Tenant(u.ID); err != nil || token != "" {
		t.Errorf("Tenant with root claims = %q, %v, want empty", token, err)
	}
}

func TestCreateLeavesNothingBehindOnFailure(t *testing.T) {
	svc, gw, tenants, _ := newTestService(t)
	tenants.Err = errors.New("sandboxd 503")
	if _, err := svc.Create(t.Context(), NewUser{Name: "qa1", DailyCapMicros: new(int64(5))}); err == nil {
		t.Fatal("Create succeeded with sandboxd down")
	}
	if len(gw.Caps) != 0 || len(svc.List()) != 0 {
		t.Errorf("after a failed Create: gw caps %v, users %v", gw.Caps, svc.List())
	}
	for _, nu := range []NewUser{{Name: "Q"}, {Name: "qa1", Password: "short"}, {Name: "qa1", ID: "bad/id"}} {
		if _, err := svc.Create(t.Context(), nu); !errors.Is(err, ErrInvalid) {
			t.Errorf("Create(%+v) = %v, want ErrInvalid", nu, err)
		}
	}
}

func TestEnsureCreatesAnOutsideUserUnderItsOwnIDOnce(t *testing.T) {
	svc, _, tenants, _ := newTestService(t)
	const id = outsideID
	u, err := svc.Ensure(t.Context(), "firebase", id)
	if err != nil || u.ID != id || u.Name != id || u.IDP != "firebase" || u.Admin {
		t.Fatalf("Ensure = %+v, %v", u, err)
	}
	token := tenants.Tokens[u.ID]
	if again, err := svc.Ensure(t.Context(), "firebase", id); err != nil || again.ID != u.ID || tenants.Tokens[u.ID] != token {
		t.Errorf("a second Ensure = %+v, %v; tenant token %q, want %q kept", again, err, tenants.Tokens[u.ID], token)
	}
	if boxes := svc.Boxes(u.ID); len(boxes) != 1 || len(tenants.Max) != 1 {
		t.Errorf("boxes %+v, tenants %v, want one of each", boxes, tenants.Max)
	}
	if _, err := svc.Ensure(t.Context(), "firebase", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("Ensure without an id = %v, want ErrInvalid", err)
	}
}

func TestEnsureNeverHandsOutAUserAnotherSourceHolds(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	local := mustCreate(t, svc, NewUser{ID: "alice", Name: "alice", Admin: true})
	if _, err := svc.Ensure(t.Context(), "other", outsideID); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, id := range []string{local.ID, outsideID} {
		if u, err := svc.Ensure(t.Context(), "firebase", id); !errors.Is(err, ErrExists) || u.ID != "" {
			t.Errorf("Ensure of %s, held by another source = %+v, %v, want ErrExists", id, u, err)
		}
		if u, err := openTestService(t, dsn, userstest.NewGW(), userstest.NewTenants()).Ensure(t.Context(), "firebase", id); !errors.Is(err, ErrExists) || u.ID != "" {
			t.Errorf("Ensure of %s, held by another source and not yet loaded = %+v, %v, want ErrExists", id, u, err)
		}
	}
}

func TestEnsureTakesOnlyAnOutsideSource(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	for _, idp := range []string{"", "password", "github"} {
		if u, err := svc.Ensure(t.Context(), idp, outsideID); !errors.Is(err, ErrInvalid) || u.ID != "" {
			t.Errorf("Ensure from source %q = %+v, %v, want ErrInvalid", idp, u, err)
		}
	}
}

func TestEnsuresOfManyNewIDsCreateAFewAtATime(t *testing.T) {
	const limit = 2
	gate := &gatedTenants{Tenants: userstest.NewTenants(), open: make(chan struct{})}
	svc, err := Open(t.Context(), Options{
		DSN: clustertest.EmptyDSN(t), MasterKeyFile: userstest.MasterKeyFile(t), GW: userstest.NewGW(), Tenants: gate,
		FirstBoxID: firstBoxID, EnsureLimit: limit,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(svc.Close)
	var wg sync.WaitGroup
	for i := range limit + 1 {
		wg.Go(func() {
			if _, err := svc.Ensure(t.Context(), "firebase", fmt.Sprintf("uid%02d", i)); err != nil {
				t.Errorf("Ensure: %v", err)
			}
		})
	}
	clustertest.WaitFor(t, "the first creates start", func() bool { return gate.active.Load() >= limit })
	time.Sleep(200 * time.Millisecond)
	if most := gate.most.Load(); most != limit {
		t.Errorf("%d creates ran at once, want %d", most, limit)
	}
	close(gate.open)
	wg.Wait()
}

func TestEnsuresRacingInTwoProcessesLeaveOneWorkingUser(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	other := openTestService(t, dsn, gw, tenants)
	const id = outsideID
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			if _, err := []*Service{svc, other}[i%2].Ensure(t.Context(), "firebase", id); err != nil {
				t.Errorf("Ensure: %v", err)
			}
		})
	}
	wg.Wait()
	if err := svc.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	token, err := svc.Tenant(id)
	if err != nil || tenants.Tokens[id] != token {
		t.Errorf("sandboxd holds tenant %q, the row %q, %v", tenants.Tokens[id], token, err)
	}
}

func TestEnsuresOnASaturatedPoolDoNotWaitOnEachOther(t *testing.T) {
	dsn := clustertest.EmptyDSN(t)
	gw, tenants := userstest.NewGW(), userstest.NewTenants()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	q := u.Query()
	q.Set("pool_max_conns", "2")
	u.RawQuery = q.Encode()
	svc := openTestService(t, u.String(), gw, tenants)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := svc.Ensure(ctx, "firebase", outsideID); err != nil {
				t.Errorf("Ensure: %v", err)
			}
		})
	}
	wg.Wait()
}

func TestCreatesOfOneIDTakeTurnsAndHandBackTheirLockConnections(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	inserts := trace(t, svc, dsn, &queryCounter{sql: "INSERT INTO users"})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := svc.createOnce(t.Context(), "firebase", outsideID); err != nil {
				t.Errorf("createOnce: %v", err)
			}
		})
	}
	wg.Wait()
	if n, held := inserts.n.Load(), svc.store.locks.Stat().AcquiredConns(); n != 1 || held != 0 {
		t.Errorf("%d user inserts, %d lock connections held after the creates, want 1 and 0", n, held)
	}
}

func TestReleasingAUserLockOnAStalledConnectionHandsTheConnectionBack(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	stall := &stalledRollback{free: make(chan struct{})}
	t.Cleanup(func() { close(stall.free) })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Tracer = stall
	locks, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open lock pool: %v", err)
	}
	svc.store.locks.Close()
	svc.store.locks = locks
	release, err := svc.store.lockUser(t.Context(), outsideID)
	if err != nil {
		t.Fatalf("lockUser: %v", err)
	}
	done := make(chan struct{})
	go func() {
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * unlockTimeout):
		t.Fatal("releasing the lock waits on a stalled rollback")
	}
	clustertest.WaitFor(t, "the stalled lock connection leaves the pool", func() bool { return locks.Stat().AcquiredConns() == 0 })
}

func TestTokensPasswordsAndDisabling(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1", Password: password})
	first, err := svc.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	if id, ok := svc.ByTokenSHA256(t.Context(), sum(first)); !ok || id != u.ID {
		t.Fatalf("token resolves to %q, %t", id, ok)
	}
	second, _ := svc.RegenerateToken(t.Context(), u.ID)
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(first)); ok {
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
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(second)); ok {
		t.Error("a disabled user's token still works")
	}
	if _, err := svc.Session(t.Context(), secret); !errors.Is(err, ErrNotFound) {
		t.Errorf("Session of a disabled user = %v", err)
	}
	if _, err := svc.Login(t.Context(), "qa1", password); !errors.Is(err, ErrBadPassword) {
		t.Errorf("Login of a disabled user = %v", err)
	}
	other := mustCreate(t, svc, NewUser{Name: "qa2"})
	if err := svc.SetTokenSHA256(t.Context(), other.ID, sum(second)); !errors.Is(err, ErrExists) {
		t.Errorf("SetTokenSHA256 with another user's hash = %v, want ErrExists", err)
	}
}

func TestLoginFindsAUserAnotherProcessAdded(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	cli := openTestService(t, dsn, gw, tenants)
	u := mustCreate(t, cli, NewUser{Name: "qa1", Admin: true})
	if err := cli.SetPassword(t.Context(), u.ID, password); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	got, err := svc.Login(t.Context(), "qa1", password)
	if err != nil || got.ID != u.ID || !got.Admin {
		t.Fatalf("Login = %+v, %v", got, err)
	}
	if boxes := svc.Boxes(u.ID); len(boxes) != 1 {
		t.Errorf("Boxes after Login = %+v, want the first box", boxes)
	}
}

func TestATokenAnotherProcessSetWorksAtOnce(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	cli := openTestService(t, dsn, gw, tenants)
	u := mustCreate(t, cli, NewUser{Name: "qa1"})
	token := TokenPrefix + utils.RandomHex(secretBytes)
	if err := cli.SetTokenSHA256(t.Context(), u.ID, sum(token)); err != nil {
		t.Fatalf("SetTokenSHA256: %v", err)
	}
	if id, ok := svc.ByTokenSHA256(t.Context(), sum(token)); !ok || id != u.ID {
		t.Fatalf("token resolves to %q, %t, want %s", id, ok, u.ID)
	}
	if boxes := svc.Boxes(u.ID); len(boxes) != 1 {
		t.Errorf("Boxes after the token resolved = %+v, want the first box", boxes)
	}
}

func TestRevokeTokenDropsTheTokenEverywhereAndEndsItsCalls(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	token, _ := svc.RegenerateToken(t.Context(), u.ID)
	device, _ := svc.IssueDevice(t.Context(), u.ID, "cocoonbox-cli", "laptop")
	elsewhere := openTestService(t, dsn, gw, tenants)
	life, deviceLife := lifetime(svc, u.ID, sum(token)), lifetime(svc, u.ID, sum(device))
	if err := svc.RevokeToken(t.Context(), u.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if got, _ := svc.Get(u.ID); got.HasToken || life.Err() == nil || deviceLife.Err() != nil {
		t.Errorf("after RevokeToken: has token %t, its lifetime %v, the device's %v", got.HasToken, life.Err(), deviceLife.Err())
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
		t.Error("a revoked token still resolves")
	}
	if err := elsewhere.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := elsewhere.ByTokenSHA256(t.Context(), sum(token)); ok {
		t.Error("a revoked token resolves on another api after its reload")
	}
	if id, ok := svc.ByTokenSHA256(t.Context(), sum(device)); !ok || id != u.ID {
		t.Errorf("the device token resolves to %q, %t after the API token's revoke", id, ok)
	}
	if err := svc.RevokeToken(t.Context(), u.ID); err != nil {
		t.Errorf("RevokeToken without a token = %v, want nil", err)
	}
}

func TestSignOutEndsEverySignInOfTheUserOnly(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	other := mustCreate(t, svc, NewUser{Name: "qa2"})
	session, _ := svc.NewSession(t.Context(), u.ID)
	otherSession, _ := svc.NewSession(t.Context(), other.ID)
	token, _ := svc.RegenerateToken(t.Context(), u.ID)
	otherToken, _ := svc.RegenerateToken(t.Context(), other.ID)
	device, _ := svc.IssueDevice(t.Context(), u.ID, "cocoonbox-cli", "laptop")
	elsewhere := openTestService(t, dsn, gw, tenants)
	life, deviceLife, otherLife := lifetime(svc, u.ID, ""), lifetime(svc, u.ID, sum(device)), lifetime(svc, other.ID, sum(otherToken))
	if err := svc.SignOut(t.Context(), u.ID); err != nil {
		t.Fatalf("SignOut: %v", err)
	}
	if _, err := svc.Session(t.Context(), session); !errors.Is(err, ErrNotFound) {
		t.Errorf("the signed-out user's session = %v, want ErrNotFound", err)
	}
	for name, held := range map[string]string{"the personal token": token, "the device token": device} {
		if _, ok := svc.ByTokenSHA256(t.Context(), sum(held)); ok {
			t.Errorf("%s resolves after SignOut", name)
		}
	}
	if got, _ := svc.Get(u.ID); got.HasToken || got.Disabled || life.Err() == nil || deviceLife.Err() == nil {
		t.Errorf("after SignOut: has token %t, disabled %t, lifetime %v, device lifetime %v", got.HasToken, got.Disabled, life.Err(), deviceLife.Err())
	}
	if devices, err := svc.Devices(t.Context(), u.ID); err != nil || len(devices) != 0 {
		t.Errorf("Devices after SignOut = %+v, %v", devices, err)
	}
	if err := elsewhere.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := elsewhere.ByTokenSHA256(t.Context(), sum(device)); ok {
		t.Error("a signed-out device token resolves on another api after its reload")
	}
	if _, err := svc.Session(t.Context(), otherSession); err != nil {
		t.Errorf("another user's session = %v", err)
	}
	if id, ok := svc.ByTokenSHA256(t.Context(), sum(otherToken)); !ok || id != other.ID || otherLife.Err() != nil {
		t.Errorf("another user's token resolves to %q, %t, its lifetime %v", id, ok, otherLife.Err())
	}
	if _, err := svc.IssueDevice(t.Context(), u.ID, "cocoonbox-cli", "laptop"); err != nil || lifetime(svc, u.ID, "").Err() != nil {
		t.Errorf("signing in again = %v, lifetime %v", err, lifetime(svc, u.ID, "").Err())
	}
}

func TestRegeneratingTheTokenDropsTheOldOneFromAListeningService(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	go svc.Listen(t.Context())
	other := openTestService(t, dsn, gw, tenants)
	u := mustCreate(t, other, NewUser{Name: "qa1"})
	old, _ := other.RegenerateToken(t.Context(), u.ID)
	clustertest.WaitFor(t, "the listening api knows the first token", func() bool { id, ok := svc.ByTokenSHA256(t.Context(), sum(old)); return ok && id == u.ID })
	if _, err := other.RegenerateToken(t.Context(), u.ID); err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	clustertest.WaitFor(t, "the listening api drops the token a regenerate replaced", func() bool { _, ok := svc.ByTokenSHA256(t.Context(), sum(old)); return !ok })
}

func TestNotifyReloadsAListeningServiceAndAReconnectReloadsWhatItMissed(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	go svc.Listen(t.Context())
	cli := openTestService(t, dsn, gw, tenants)
	u := mustCreate(t, cli, NewUser{Name: "qa1"})
	old := TokenPrefix + utils.RandomHex(secretBytes)
	if err := cli.SetTokenSHA256(t.Context(), u.ID, sum(old)); err != nil {
		t.Fatalf("SetTokenSHA256: %v", err)
	}
	clustertest.WaitFor(t, "the user another process added is known", func() bool { _, err := svc.Get(u.ID); return err == nil })
	if id, ok := svc.ByTokenSHA256(t.Context(), sum(old)); !ok || id != u.ID {
		t.Fatalf("old token resolves to %q, %t", id, ok)
	}

	replaced := TokenPrefix + utils.RandomHex(secretBytes)
	if err := cli.SetTokenSHA256(t.Context(), u.ID, sum(replaced)); err != nil {
		t.Fatalf("SetTokenSHA256: %v", err)
	}
	clustertest.WaitFor(t, "the replaced token stops working", func() bool { _, ok := svc.ByTokenSHA256(t.Context(), sum(old)); return !ok })

	if err := userstest.Exec(t, dsn, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'LISTEN%'"); err != nil {
		t.Fatalf("end the listening connection: %v", err)
	}
	missed := TokenPrefix + utils.RandomHex(secretBytes)
	if err := cli.SetTokenSHA256(t.Context(), u.ID, sum(missed)); err != nil {
		t.Fatalf("SetTokenSHA256: %v", err)
	}
	clustertest.WaitFor(t, "a token replaced while the listener was down stops working", func() bool { _, ok := svc.ByTokenSHA256(t.Context(), sum(replaced)); return !ok })
}

func TestUnknownTokensCheckTheTableAtMostOncePerInterval(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	queries := trace(t, svc, dsn, &queryCounter{})
	var calls atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for range 8 {
		wg.Go(func() {
			for time.Since(start) < 3*tokenMissInterval+tokenMissInterval/2 {
				calls.Add(1)
				if _, ok := svc.ByTokenSHA256(t.Context(), sum(utils.RandomHex(secretBytes))); ok {
					t.Error("a garbage token resolved")
				}
			}
		})
	}
	wg.Wait()
	bound := int64(time.Since(start)/tokenMissInterval) + 1
	if got := queries.n.Load(); got < 1 || got > bound {
		t.Errorf("%d garbage lookups ran %d queries, want 1..%d", calls.Load(), got, bound)
	}
}

func TestUpdateKeepsBoxesUnderTheCapAndMovesTheTenant(t *testing.T) {
	svc, _, tenants, _ := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
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
	if tenants.Max[u.ID] != 3 || !svc.Residential(u.ID) {
		t.Errorf("tenant cap %d, residential %t", tenants.Max[u.ID], svc.Residential(u.ID))
	}
	if got, _ := svc.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(1)}); got.MaxBoxes != 1 || tenants.Max[u.ID] != 2 {
		t.Errorf("lowering the cap left max boxes %d, tenant cap %d", got.MaxBoxes, tenants.Max[u.ID])
	}
	if svc.Residential("u-unknown") || len(flips) != 1 || !flips[0] {
		t.Errorf("residential flips = %v", flips)
	}
}

func TestDeleteRemovesTenantKeyRowAndSessions(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1", Password: password})
	token, _ := svc.RegenerateToken(t.Context(), u.ID)
	if _, err := svc.NewSession(t.Context(), u.ID); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := svc.Delete(t.Context(), u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !gw.Revoked[u.ID] {
		t.Error("gw keys kept")
	}
	if _, ok := tenants.Max[u.ID]; ok {
		t.Error("tenant kept")
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
		t.Error("the deleted user's token still works")
	}
	var sessions int
	if err := userstest.Query(t, dsn, "SELECT count(*) FROM sessions").Scan(&sessions); err != nil || sessions != 0 {
		t.Errorf("sessions left = %d, %v", sessions, err)
	}
	if err := svc.Delete(t.Context(), u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestASealedValueOpensOnlyOnItsOwnRow(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	a := mustCreate(t, svc, NewUser{Name: "alice"})
	b := mustCreate(t, svc, NewUser{Name: "bob"})
	if err := userstest.Exec(t, dsn, "UPDATE users SET tenant_token_sealed = (SELECT tenant_token_sealed FROM users WHERE id = $1) WHERE id = $2", a.ID, b.ID); err != nil {
		t.Fatalf("copy sealed token: %v", err)
	}
	reopened := openTestService(t, dsn, gw, tenants)
	if _, err := reopened.Tenant(b.ID); err == nil {
		t.Error("alice's sealed tenant token opened as bob's")
	}
	if _, err := reopened.StateKey(t.Context(), b.ID); err != nil {
		t.Errorf("bob's own state key: %v", err)
	}
}

func TestStateKeyIsSealedToItsUser(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	created, err := svc.StateKey(t.Context(), u.ID)
	if err != nil || len(created.Secret) != keyring.KeySize || created.ID == "" {
		t.Fatalf("StateKey of a new user = %d bytes, id %q, %v", len(created.Secret), created.ID, err)
	}
	var sealed []byte
	if err := userstest.Query(t, dsn, "SELECT state_key_sealed FROM users WHERE id = $1", u.ID).Scan(&sealed); err != nil || bytes.Contains(sealed, created.Secret) {
		t.Errorf("state key column holds the key in the clear or is unreadable: %v", err)
	}

	other := mustCreate(t, svc, NewUser{Name: "qa2"})
	if err := userstest.Exec(t, dsn, "UPDATE users SET state_key_sealed = (SELECT state_key_sealed FROM users WHERE id = $1) WHERE id = $2", u.ID, other.ID); err != nil {
		t.Fatalf("copy sealed state key: %v", err)
	}
	if _, err := openTestService(t, dsn, gw, tenants).StateKey(t.Context(), other.ID); err == nil {
		t.Error("qa1's sealed state key opened as qa2's")
	}
}

func TestAdminsHaveNoGWCapAndDeleteDropsTheCap(t *testing.T) {
	svc, gw, _, _ := newTestService(t)
	admin := mustCreate(t, svc, NewUser{Name: "root", Admin: true})
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	if caps, _ := svc.DailyCaps(t.Context()); !maps.Equal(caps, map[string]int64{admin.ID: gwkeys.Unlimited}) {
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
	if _, ok := gw.Caps[u.ID]; ok {
		t.Error("the deleted user's gw cap stayed")
	}
}

func TestNewBoxesTakeTheConfiguredFlavorAndKeepItWhenTheFlavorChanges(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(3), MaxBoxes: new(3)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := svc.AddBox(t.Context(), Box{ID: "cb-000000000001", Owner: u.ID, Name: "second"}); err != nil {
		t.Fatalf("AddBox: %v", err)
	}
	later := openFlavored(t, dsn, gw, tenants, newFlavor)
	if _, err := later.AddBox(t.Context(), Box{ID: "cb-000000000002", Owner: u.ID, Name: "third"}); err != nil {
		t.Fatalf("AddBox on the new flavor: %v", err)
	}
	v := mustCreate(t, later, NewUser{Name: "qa2"})
	got := map[string]machines.Flavor{}
	for _, b := range append(later.Boxes(u.ID), later.Boxes(v.ID)...) {
		got[b.Name+"@"+b.Owner] = b.Flavor
	}
	want := map[string]machines.Flavor{
		FirstBoxName + "@" + u.ID: oldFlavor, "second@" + u.ID: oldFlavor,
		"third@" + u.ID: newFlavor, FirstBoxName + "@" + v.ID: newFlavor,
	}
	if !maps.Equal(got, want) {
		t.Errorf("box flavors = %v, want %v", got, want)
	}
}

func TestTheBoxFlavorMigrationGivesEveryExistingBoxTheOldFlavorAndNewRowsNone(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	svc.Close()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	for _, sql := range []string{
		"ALTER TABLE boxes DROP COLUMN vcpus, DROP COLUMN memory_mib, DROP COLUMN disk_gib",
		"DELETE FROM schema_migrations WHERE version = 15",
	} {
		if _, err := conn.Exec(t.Context(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	reopened := openFlavored(t, dsn, gw, tenants, newFlavor)
	if boxes := reopened.Boxes(u.ID); len(boxes) != 1 || boxes[0].Flavor != oldFlavor {
		t.Errorf("boxes after the migration = %+v, want the one box on %+v", boxes, oldFlavor)
	}
	if _, err := conn.Exec(t.Context(), "INSERT INTO boxes (id, owner, name) VALUES ('cb-000000000009', $1, 'bare')", u.ID); err == nil {
		t.Error("a box row without a flavor was inserted, want the column without a default to refuse it")
	}
}

func TestDisablingTurnsOffTheGWKeyAndEndsTheUsersLifetime(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	other := openTestService(t, dsn, gw, tenants)
	life, elsewhere := lifetime(svc, u.ID, ""), lifetime(other, u.ID, "")
	if life.Err() != nil || elsewhere.Err() != nil {
		t.Fatalf("an enabled user's lifetime ended: %v, %v", life.Err(), elsewhere.Err())
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !gw.Disabled[u.ID] || life.Err() == nil || lifetime(svc, u.ID, "").Err() == nil {
		t.Errorf("after disable: gw key off %t, lifetime %v, a new lifetime %v", gw.Disabled[u.ID], life.Err(), lifetime(svc, u.ID, "").Err())
	}
	if err := other.Load(t.Context()); err != nil || elsewhere.Err() == nil {
		t.Errorf("another api reloading the disable = %v, its lifetime %v", err, elsewhere.Err())
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: new(false)}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	life = lifetime(svc, u.ID, "")
	if gw.Disabled[u.ID] || life.Err() != nil {
		t.Errorf("after enable: gw key off %t, lifetime %v", gw.Disabled[u.ID], life.Err())
	}
	if err := svc.Delete(t.Context(), u.ID); err != nil || life.Err() == nil {
		t.Errorf("Delete = %v, lifetime %v", err, life.Err())
	}
}

func TestReconcileTenantsWritesOnlyTheMissingAndChangedTenantsWithTheirTokens(t *testing.T) {
	svc, _, tenants, _ := newTestService(t)
	kept := mustCreate(t, svc, NewUser{Name: "qa1"})
	lost := mustCreate(t, svc, NewUser{Name: "qa2"})
	moved := mustCreate(t, svc, NewUser{Name: "qa3"})
	token := tenants.Tokens[lost.ID]
	delete(tenants.Max, lost.ID)
	delete(tenants.Tokens, lost.ID)
	tenants.Max[moved.ID] = 9
	tenants.Tokens[kept.ID] = "untouched"
	n, err := svc.reconcileTenants(t.Context())
	if err != nil || n != 2 {
		t.Fatalf("reconcileTenants = %d, %v; want the 2 stale tenants", n, err)
	}
	if tenants.Tokens[lost.ID] != token || tenants.Max[moved.ID] != tenants.Max[kept.ID] || tenants.Tokens[kept.ID] != "untouched" {
		t.Errorf("tenants after reconcile: max %v, lost token restored %t, kept token %q", tenants.Max, tenants.Tokens[lost.ID] == token, tenants.Tokens[kept.ID])
	}
	if n, err := svc.reconcileTenants(t.Context()); err != nil || n != 0 {
		t.Errorf("a second reconcileTenants = %d, %v; want nothing stale", n, err)
	}
}

func TestPromotingAndDemotingMovesTheGWCapAndTheLastAdminStays(t *testing.T) {
	svc, gw, _, dsn := newTestService(t)
	root := mustCreate(t, svc, NewUser{Name: "root", Admin: true})
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	if got, err := svc.Update(t.Context(), u.ID, Settings{Admin: new(true)}); err != nil || !got.Admin || gw.Caps[u.ID] != gwkeys.Unlimited {
		t.Fatalf("promote = %+v, %v; gw cap %v", got, err, gw.Caps[u.ID])
	}
	if got, err := openTestService(t, dsn, gw, userstest.NewTenants()).Get(u.ID); err != nil || !got.Admin {
		t.Errorf("a fresh service reads %+v, %v; want the promotion stored", got, err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := svc.Update(t.Context(), root.ID, Settings{Admin: new(false)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("demoting the last enabled admin = %v, want ErrInvalid", err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: new(false)}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got, err := svc.Update(t.Context(), root.ID, Settings{Admin: new(false)}); err != nil || got.Admin {
		t.Fatalf("demote with another admin = %+v, %v", got, err)
	}
	if _, ok := gw.Caps[root.ID]; ok {
		t.Errorf("a demoted admin keeps gw cap %d, want the tenant's default", gw.Caps[root.ID])
	}
}

type gatedTenants struct {
	*userstest.Tenants
	open         chan struct{}
	active, most atomic.Int32
}

func (g *gatedTenants) PutTenant(ctx context.Context, name, token string, maxClaims int) error {
	n := g.active.Add(1)
	defer g.active.Add(-1)
	for m := g.most.Load(); n > m && !g.most.CompareAndSwap(m, n); m = g.most.Load() {
	}
	<-g.open
	return g.Tenants.PutTenant(ctx, name, token, maxClaims)
}

type stalledRollback struct {
	free chan struct{}
}

func (g *stalledRollback) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == "rollback" {
		select {
		case <-ctx.Done():
		case <-g.free:
		}
	}
	return ctx
}

func (*stalledRollback) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type queryCounter struct {
	sql string
	n   atomic.Int64
}

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, c.sql) {
		c.n.Add(1)
	}
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func newTestService(t *testing.T) (*Service, *userstest.GW, *userstest.Tenants, string) {
	t.Helper()
	dsn := clustertest.EmptyDSN(t)
	gw, tenants := userstest.NewGW(), userstest.NewTenants()
	return openTestService(t, dsn, gw, tenants), gw, tenants, dsn
}

func openTestService(t *testing.T, dsn string, gw GWKeys, tenants machines.Tenants) *Service {
	t.Helper()
	return openFlavored(t, dsn, gw, tenants, oldFlavor)
}

func openFlavored(t *testing.T, dsn string, gw GWKeys, tenants machines.Tenants, flavor machines.Flavor) *Service {
	t.Helper()
	svc, err := Open(t.Context(), Options{DSN: dsn, MasterKeyFile: userstest.MasterKeyFile(t), GW: gw, Tenants: tenants, TenantClaims: true, FirstBoxID: firstBoxID, EnsureLimit: 4, BoxFlavor: flavor})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

func trace[T pgx.QueryTracer](t *testing.T, svc *Service, dsn string, tracer T) T {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	svc.store.pool.Close()
	svc.store.pool = pool
	return tracer
}

func mustCreate(t *testing.T, svc *Service, nu NewUser) User {
	t.Helper()
	u, err := svc.Create(t.Context(), nu)
	if err != nil {
		t.Fatalf("create %s: %v", nu.Name, err)
	}
	return u
}

func lifetime(svc *Service, id, credential string) context.Context {
	ctx, _ := svc.Lifetime(id, credential)
	return ctx
}

func sum(token string) string {
	return utils.HashHex(token)
}
