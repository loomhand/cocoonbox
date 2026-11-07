package users

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
)

func TestLoadCannotRestoreRevokedTokens(t *testing.T) {
	for name, remote := range map[string]bool{"local revoke": false, "newer reload": true} {
		t.Run(name, func(t *testing.T) {
			svc, gw, tenants, dsn := newTestService(t)
			u := mustCreate(t, svc, NewUser{Name: "alice"})
			token, err := svc.RegenerateToken(t.Context(), u.ID)
			if err != nil {
				t.Fatal(err)
			}
			writer := svc
			if remote {
				writer = openTestService(t, dsn, gw, tenants)
			}
			gate := gateQuery(t, svc, dsn, "FROM user_tokens ORDER BY")
			done := make(chan error, 1)
			go func() { done <- svc.Load(t.Context()) }()
			<-gate.entered
			err = writer.RevokeToken(t.Context(), u.ID)
			if err == nil && remote {
				err = svc.Load(t.Context())
			}
			close(gate.resume)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
				t.Fatal("the delayed snapshot restored the revoked token")
			}
			if lifetime(svc, u.ID, sum(token)).Err() == nil {
				t.Fatal("the revoked token opened a new lifetime")
			}
		})
	}
}

func TestAnUpdateCannotRestoreATokenRevokedWhileItWaited(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "alice"})
	token, err := svc.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate := gateQuery(t, svc, dsn, "UPDATE users SET max_boxes")
	done := make(chan error, 1)
	go func() {
		_, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(2)})
		done <- err
	}()
	<-gate.entered
	err = svc.RevokeToken(t.Context(), u.ID)
	close(gate.resume)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
		t.Fatal("the settings update restored the revoked token")
	}
}

func TestAnIssuedDeviceCannotOutliveASignOutWhileItWaited(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "alice"})
	gate := gateQuery(t, svc, dsn, "SELECT pg_notify")
	done := make(chan error, 1)
	var token string
	go func() {
		var err error
		token, err = svc.IssueDevice(t.Context(), u.ID, "app", "laptop")
		done <- err
	}()
	<-gate.entered
	err := svc.SignOut(t.Context(), u.ID)
	close(gate.resume)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
		t.Fatal("the delayed issue restored the signed-out device")
	}
}

func TestAListenerReadsANewUserWithoutAFullReload(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	loads := trace(t, svc, dsn, &queryCounter{sql: "FROM users ORDER BY"})
	go svc.Listen(t.Context())
	clustertest.WaitFor(t, "the listener loads once", func() bool { return loads.n.Load() == 1 })
	creator := openTestService(t, dsn, gw, tenants)
	creatorLoads := trace(t, creator, dsn, &queryCounter{sql: "FROM users ORDER BY"})
	u, err := creator.Ensure(t.Context(), "firebase", outsideID)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	clustertest.WaitFor(t, "the listener knows the new user", func() bool { _, err := svc.Get(u.ID); return err == nil })
	if got, err := svc.ByName(u.Name); err != nil || got.ID != u.ID || len(svc.Boxes(u.ID)) != 1 || len(creator.Boxes(u.ID)) != 1 {
		t.Errorf("listener ByName = %+v, %v; boxes %+v on the listener, %+v on the creator", got, err, svc.Boxes(u.ID), creator.Boxes(u.ID))
	}
	if n, m := loads.n.Load(), creatorLoads.n.Load(); n != 1 || m != 0 {
		t.Errorf("full reloads after the create: listener %d, creator %d, want none", n-1, m)
	}
}

func TestACreateRacingAFullLoadKeepsTheNewUser(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	gate := gateQuery(t, svc, dsn, "FROM boxes ORDER BY")
	done := make(chan error, 1)
	go func() { done <- svc.Load(t.Context()) }()
	<-gate.entered
	u, err := svc.Ensure(t.Context(), "firebase", outsideID)
	close(gate.resume)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := svc.Get(u.ID); err != nil || len(svc.Boxes(u.ID)) != 1 {
		t.Errorf("after the delayed Load: Get = %v, boxes %+v", err, svc.Boxes(u.ID))
	}
}

func TestAListenerFullyReloadsTokenChangesAndDisables(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	go svc.Listen(t.Context())
	other := openTestService(t, dsn, gw, tenants)
	u := mustCreate(t, other, NewUser{Name: "qa1"})
	token, err := other.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	clustertest.WaitFor(t, "the listener knows the token", func() bool { _, ok := svc.indexedToken(sum(token)); return ok })
	tokenLife := lifetime(svc, u.ID, sum(token))
	if _, err := other.RegenerateToken(t.Context(), u.ID); err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	clustertest.WaitFor(t, "the listener ends the replaced token's calls", func() bool { return tokenLife.Err() != nil })
	life := lifetime(svc, u.ID, "")
	if _, err := other.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	clustertest.WaitFor(t, "the listener ends the disabled user's lifetime", func() bool { return life.Err() != nil })
}

func TestAFullLoadInstallsWhileNewUsersKeepArriving(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	elsewhere := mustCreate(t, openTestService(t, dsn, gw, tenants), NewUser{Name: "qa1"})
	signups := trace(t, svc, dsn, &signupDuringLoad{svc: svc})
	if err := svc.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := signups.loads.Load(); n != 1 || len(signups.ids) != 1 {
		t.Fatalf("Load read the tables %d times while %d users signed up, want once", n, len(signups.ids))
	}
	for _, id := range append(signups.ids, elsewhere.ID) {
		if _, err := svc.Get(id); err != nil || len(svc.Boxes(id)) != 1 {
			t.Errorf("after the Load: Get(%s) = %v, boxes %+v", id, err, svc.Boxes(id))
		}
	}
}

type reloadGate struct {
	sql     string
	entered chan struct{}
	resume  chan struct{}
	blocked atomic.Bool
}

func (g *reloadGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, g.sql) && g.blocked.CompareAndSwap(false, true) {
		close(g.entered)
		<-g.resume
	}
	return ctx
}

func (*reloadGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func gateQuery(t *testing.T, svc *Service, dsn, sql string) *reloadGate {
	t.Helper()
	return trace(t, svc, dsn, &reloadGate{sql: sql, entered: make(chan struct{}), resume: make(chan struct{})})
}

type signupDuringLoad struct {
	svc   *Service
	loads atomic.Int64
	ids   []string
}

func (g *signupDuringLoad) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(data.SQL, "FROM boxes ORDER BY") {
		return ctx
	}
	if n := g.loads.Add(1); n <= 5 {
		id := fmt.Sprintf("signup%d", n)
		if _, err := g.svc.Ensure(ctx, "firebase", id); err == nil {
			g.ids = append(g.ids, id)
		}
	}
	return ctx
}

func (*signupDuringLoad) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
