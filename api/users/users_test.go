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
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loomhand/cocoonbox/api/gwkeys"
	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/utils"
)

const password = "correct horse battery"

func TestAnUpdateFromAStaleCacheKeepsTheColumnsItDoesNotSet(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u, err := svc.Create(t.Context(), NewUser{Name: "qa1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := exec(t, dsn, "UPDATE users SET no_gc = true, image = 'cbcore:v1' WHERE id = '"+u.ID+"'"); err != nil {
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
	u, err := svc.Create(t.Context(), NewUser{Name: "qa1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
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
	park := false
	if _, err := svc.Update(t.Context(), u.ID, Settings{NoPark: &park}); err != nil {
		t.Fatalf("Update of one field: %v", err)
	}
	if want.NoPark = false; svc.Policy(u.ID) != want {
		t.Errorf("policy after an update of one field = %+v, want %+v", svc.Policy(u.ID), want)
	}
}

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

func TestEnsureCreatesAnOutsideUserUnderItsOwnIDOnce(t *testing.T) {
	svc, gw, tenants, _ := newTestService(t)
	const id = "Fb7xQ2kLm9PzR4tVw1YcN8sJd3Hq"
	u, err := svc.Ensure(t.Context(), "firebase", id)
	if err != nil || u.ID != id || u.Name != id || u.IDP != "firebase" || u.Admin {
		t.Fatalf("Ensure = %+v, %v", u, err)
	}
	key := gw.keys[u.ID]
	if again, err := svc.Ensure(t.Context(), "firebase", id); err != nil || again.ID != u.ID || gw.keys[u.ID] != key {
		t.Errorf("a second Ensure = %+v, %v; gw key %q, want %q kept", again, err, gw.keys[u.ID], key)
	}
	if boxes := svc.Boxes(u.ID); len(boxes) != 1 || len(tenants.tenants) != 1 {
		t.Errorf("boxes %+v, tenants %v, want one of each", boxes, tenants.tenants)
	}
	if _, err := svc.Ensure(t.Context(), "firebase", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("Ensure without an id = %v, want ErrInvalid", err)
	}
}

func TestEnsureNeverHandsOutAUserAnotherSourceHolds(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	local, err := svc.Create(t.Context(), NewUser{ID: "alice", Name: "alice", Admin: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Ensure(t.Context(), "other", "Fb7xQ2kLm9PzR4tVw1YcN8sJd3Hq"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, id := range []string{local.ID, "Fb7xQ2kLm9PzR4tVw1YcN8sJd3Hq"} {
		if u, err := svc.Ensure(t.Context(), "firebase", id); !errors.Is(err, ErrExists) || u.ID != "" {
			t.Errorf("Ensure of %s, held by another source = %+v, %v, want ErrExists", id, u, err)
		}
		if u, err := openTestService(t, dsn, &fakeGW{keys: map[string]string{}, caps: map[string]int64{}, disabled: map[string]bool{}}, &fakeTenants{tenants: map[string]tenant{}}).Ensure(t.Context(), "firebase", id); !errors.Is(err, ErrExists) || u.ID != "" {
			t.Errorf("Ensure of %s, held by another source and not yet loaded = %+v, %v, want ErrExists", id, u, err)
		}
	}
}

func TestEnsureTakesOnlyAnOutsideSource(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	for _, idp := range []string{"", "password", "github"} {
		if u, err := svc.Ensure(t.Context(), idp, "Fb7xQ2kLm9PzR4tVw1YcN8sJd3Hq"); !errors.Is(err, ErrInvalid) || u.ID != "" {
			t.Errorf("Ensure from source %q = %+v, %v, want ErrInvalid", idp, u, err)
		}
	}
}

func TestEnsuresOfManyNewIDsCreateAFewAtATime(t *testing.T) {
	dsn := freshDatabase(t)
	gw := &gatedGW{fakeGW: fakeGW{keys: map[string]string{}, caps: map[string]int64{}, disabled: map[string]bool{}}, open: make(chan struct{})}
	svc := openTestService(t, dsn, gw, &fakeTenants{tenants: map[string]tenant{}})
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			_, err := svc.Ensure(t.Context(), "firebase", fmt.Sprintf("uid%02d", i))
			errs <- err
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for gw.active.Load() < ensureLimit && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if most := gw.most.Load(); most != ensureLimit {
		t.Errorf("%d creates ran at once, want %d", most, ensureLimit)
	}
	close(gw.open)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Ensure: %v", err)
		}
	}
}

func TestEnsuresRacingInTwoProcessesLeaveOneWorkingUser(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	other := openTestService(t, dsn, gw, tenants)
	const id = "Fb7xQ2kLm9PzR4tVw1YcN8sJd3Hq"
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			_, err := []*Service{svc, other}[i%2].Ensure(t.Context(), "firebase", id)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Ensure: %v", err)
		}
	}
	if err := svc.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	token, err := svc.Tenant(id)
	if err != nil || tenants.tenants[id].token != token {
		t.Errorf("sandboxd holds tenant %+v, the row %q, %v", tenants.tenants[id], token, err)
	}
	if key, err := svc.GWKey(id); err != nil || gw.keys[id] != key {
		t.Errorf("gw holds key %q, the row %q, %v", gw.keys[id], key, err)
	}
}

func TestEnsuresOnASaturatedPoolDoNotWaitOnEachOther(t *testing.T) {
	dsn := freshDatabase(t)
	gw := &fakeGW{keys: map[string]string{}, caps: map[string]int64{}, disabled: map[string]bool{}}
	tenants := &fakeTenants{tenants: map[string]tenant{}}
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
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := svc.Ensure(ctx, "firebase", "Fb7xQ2kLm9PzR4tVw1YcN8sJd3Hq")
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Ensure: %v", err)
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
	disabled := true
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: &disabled}); err != nil {
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
	other, _ := svc.Create(t.Context(), NewUser{Name: "qa2"})
	if err := svc.SetTokenSHA256(t.Context(), other.ID, sum(second)); !errors.Is(err, ErrExists) {
		t.Errorf("SetTokenSHA256 with another user's hash = %v, want ErrExists", err)
	}
}

func TestLoginFindsAUserAnotherProcessAdded(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	cli := openTestService(t, dsn, gw, tenants)
	u, err := cli.Create(t.Context(), NewUser{Name: "qa1", Admin: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
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
	u, err := cli.Create(t.Context(), NewUser{Name: "qa1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
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
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
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
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	other, _ := svc.Create(t.Context(), NewUser{Name: "qa2"})
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
	console := openTestService(t, dsn, gw, tenants)
	u, _ := console.Create(t.Context(), NewUser{Name: "qa1"})
	old, _ := console.RegenerateToken(t.Context(), u.ID)
	if took, ok := within(time.Second, func() bool { id, ok := svc.ByTokenSHA256(t.Context(), sum(old)); return ok && id == u.ID }); !ok {
		t.Fatalf("the listening api does not know the first token %s after it was set", took)
	}
	if _, err := console.RegenerateToken(t.Context(), u.ID); err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	if took, ok := within(time.Second, func() bool { _, ok := svc.ByTokenSHA256(t.Context(), sum(old)); return !ok }); !ok {
		t.Errorf("the listening api still takes the replaced token %s after the regenerate", took)
	}
}

func TestNotifyReloadsAListeningServiceAndAReconnectReloadsWhatItMissed(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	go svc.Listen(t.Context())
	cli := openTestService(t, dsn, gw, tenants)
	u, err := cli.Create(t.Context(), NewUser{Name: "qa1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	old := TokenPrefix + utils.RandomHex(secretBytes)
	if err := cli.SetTokenSHA256(t.Context(), u.ID, sum(old)); err != nil {
		t.Fatalf("SetTokenSHA256: %v", err)
	}
	if took, ok := within(time.Second, func() bool { _, err := svc.Get(u.ID); return err == nil }); !ok {
		t.Fatalf("the user another process added is unknown %s after the write", took)
	}
	if id, ok := svc.ByTokenSHA256(t.Context(), sum(old)); !ok || id != u.ID {
		t.Fatalf("old token resolves to %q, %t", id, ok)
	}

	replaced := TokenPrefix + utils.RandomHex(secretBytes)
	if err := cli.SetTokenSHA256(t.Context(), u.ID, sum(replaced)); err != nil {
		t.Fatalf("SetTokenSHA256: %v", err)
	}
	took, ok := within(time.Second, func() bool { _, ok := svc.ByTokenSHA256(t.Context(), sum(old)); return !ok })
	if !ok {
		t.Fatalf("the replaced token still works %s after the write", took)
	}
	t.Logf("the replaced token stopped working %s after the write", took)

	if err := exec(t, dsn, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'LISTEN%'"); err != nil {
		t.Fatalf("end the listening connection: %v", err)
	}
	missed := TokenPrefix + utils.RandomHex(secretBytes)
	if err := cli.SetTokenSHA256(t.Context(), u.ID, sum(missed)); err != nil {
		t.Fatalf("SetTokenSHA256: %v", err)
	}
	if took, ok := within(listenRetry+time.Second, func() bool { _, ok := svc.ByTokenSHA256(t.Context(), sum(replaced)); return !ok }); !ok {
		t.Errorf("a token replaced while the listener was down still works %s later", took)
	}
}

func TestUnknownTokensCheckTheTableAtMostOncePerInterval(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	queries := traceQueries(t, svc, dsn)
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
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
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

func TestDisablingTurnsOffTheGWKeyAndEndsTheUsersLifetime(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	other := openTestService(t, dsn, gw, tenants)
	life, elsewhere := lifetime(svc, u.ID, ""), lifetime(other, u.ID, "")
	if life.Err() != nil || elsewhere.Err() != nil {
		t.Fatalf("an enabled user's lifetime ended: %v, %v", life.Err(), elsewhere.Err())
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !gw.disabled[u.ID] || life.Err() == nil || lifetime(svc, u.ID, "").Err() == nil {
		t.Errorf("after disable: gw key off %t, lifetime %v, a new lifetime %v", gw.disabled[u.ID], life.Err(), lifetime(svc, u.ID, "").Err())
	}
	if err := other.Load(t.Context()); err != nil || elsewhere.Err() == nil {
		t.Errorf("another api reloading the disable = %v, its lifetime %v", err, elsewhere.Err())
	}
	clear(gw.disabled)
	if err := svc.Reconcile(t.Context()); err != nil || !gw.disabled[u.ID] {
		t.Errorf("Reconcile = %v, the disabled user's gw key off %t", err, gw.disabled[u.ID])
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: new(false)}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	life = lifetime(svc, u.ID, "")
	if gw.disabled[u.ID] || life.Err() != nil {
		t.Errorf("after enable: gw key off %t, lifetime %v", gw.disabled[u.ID], life.Err())
	}
	if err := svc.Delete(t.Context(), u.ID); err != nil || life.Err() == nil {
		t.Errorf("Delete = %v, lifetime %v", err, life.Err())
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

func TestPromotingAndDemotingMovesTheGWCapAndTheLastAdminStays(t *testing.T) {
	svc, gw, _, dsn := newTestService(t)
	root, _ := svc.Create(t.Context(), NewUser{Name: "root", Admin: true})
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	if got, err := svc.Update(t.Context(), u.ID, Settings{Admin: new(true)}); err != nil || !got.Admin || gw.caps[u.ID] != gwkeys.Unlimited {
		t.Fatalf("promote = %+v, %v; gw cap %v", got, err, gw.caps[u.ID])
	}
	if got, err := openTestService(t, dsn, gw, &fakeTenants{tenants: map[string]tenant{}}).Get(u.ID); err != nil || !got.Admin {
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
	if _, ok := gw.caps[root.ID]; ok {
		t.Errorf("a demoted admin keeps gw cap %d, want the tenant's default", gw.caps[root.ID])
	}
}

// gatedGW holds every Add until open closes and counts the Adds running at once.
type gatedGW struct {
	fakeGW
	open         chan struct{}
	active, most atomic.Int32
}

func (g *gatedGW) Add(ctx context.Context, owner string) (string, string, error) {
	n := g.active.Add(1)
	defer g.active.Add(-1)
	for m := g.most.Load(); n > m && !g.most.CompareAndSwap(m, n); m = g.most.Load() {
	}
	<-g.open
	return g.fakeGW.Add(ctx, owner)
}

type fakeGW struct {
	mu       sync.Mutex
	keys     map[string]string
	caps     map[string]int64
	disabled map[string]bool
}

func (g *fakeGW) Add(_ context.Context, owner string) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.keys[owner] = "gw-" + utils.RandomHex(8)
	return g.keys[owner], "sha256:" + owner, nil
}

func (g *fakeGW) Revoke(_ context.Context, owner string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.keys, owner)
	return nil
}

func (g *fakeGW) SetEnabled(_ context.Context, owner string, enabled bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.disabled[owner] = !enabled
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

type queryCounter struct {
	n atomic.Int64
}

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type tenant struct {
	token string
	max   int
}

type fakeTenants struct {
	mu      sync.Mutex
	tenants map[string]tenant
	err     error
}

func (f *fakeTenants) PutTenant(_ context.Context, name, token string, maxClaims int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.tenants[name] = tenant{token: cmp.Or(token, f.tenants[name].token), max: maxClaims}
	return nil
}

func (f *fakeTenants) DeleteTenant(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tenants, name)
	return nil
}

// newTestService opens a Service on a fresh database of the server COCOONBOX_TEST_DSN names.
func newTestService(t *testing.T) (*Service, *fakeGW, *fakeTenants, string) {
	t.Helper()
	dsn := freshDatabase(t)
	gw := &fakeGW{keys: map[string]string{}, caps: map[string]int64{}, disabled: map[string]bool{}}
	tenants := &fakeTenants{tenants: map[string]tenant{}}
	return openTestService(t, dsn, gw, tenants), gw, tenants, dsn
}

func openTestService(t *testing.T, dsn string, gw GWKeys, tenants *fakeTenants) *Service {
	t.Helper()
	svc, err := Open(t.Context(), Options{DSN: dsn, MasterKeyFile: masterKeyFile(t), GW: gw, Tenants: tenants, TenantClaims: true, FirstBoxID: firstBoxID})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

func traceQueries(t *testing.T, svc *Service, dsn string) *queryCounter {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	counter := &queryCounter{}
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	svc.store.pool.Close()
	svc.store.pool = pool
	return counter
}

func masterKeyFile(t *testing.T) string {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("k", masterKeySize)), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return keyFile
}

func freshDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("COCOONBOX_TEST_DSN")
	if base == "" {
		t.Skip("COCOONBOX_TEST_DSN names no PostgreSQL server")
	}
	name := "t_" + utils.RandomHex(6)
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

func within(limit time.Duration, cond func() bool) (time.Duration, bool) {
	start := time.Now()
	for !cond() {
		if time.Since(start) > limit {
			return time.Since(start), false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start), true
}

func lifetime(svc *Service, id, credential string) context.Context {
	ctx, _ := svc.Lifetime(id, credential)
	return ctx
}

func sum(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}
