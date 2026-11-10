package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"

	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/users/userstest"
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
	gate := gateQuery(t, svc, dsn, "FROM users ORDER BY")
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
	creatorLoads, creatorSyncs := &queryCounter{sql: "FROM users ORDER BY"}, &queryCounter{sql: "FROM shares WHERE revoked_at IS NULL AND"}
	trace(t, creator, dsn, multitracer.New(creatorLoads, creatorSyncs))
	go creator.Listen(t.Context())
	clustertest.WaitFor(t, "the creator's listener loads once", func() bool { return creatorSyncs.n.Load() == 1 })
	u, err := creator.Ensure(t.Context(), "firebase", outsideID)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	clustertest.WaitFor(t, "the listener knows the new user", func() bool { _, err := svc.Get(u.ID); return err == nil })
	if got, err := svc.ByName(u.Name); err != nil || got.ID != u.ID || len(svc.Boxes(u.ID)) != 1 || len(creator.Boxes(u.ID)) != 1 {
		t.Errorf("listener ByName = %+v, %v; boxes %+v on the listener, %+v on the creator", got, err, svc.Boxes(u.ID), creator.Boxes(u.ID))
	}
	if n := loads.n.Load(); n != 1 {
		t.Errorf("the listener reloaded fully %d times for the create, want none", n-1)
	}
	if err := pgx.BeginFunc(t.Context(), creator.store.pool, func(tx pgx.Tx) error { return notifyIn(t.Context(), tx, "") }); err != nil {
		t.Fatalf("notify: %v", err)
	}
	clustertest.WaitFor(t, "the creator's listener reloads on the later write", func() bool { return creatorSyncs.n.Load() == 2 })
	if m := creatorLoads.n.Load(); m != 2 {
		t.Errorf("the creator reloaded fully %d times for its own create, want none", m-2)
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

func TestAccountDisabledReadsAnotherReplicasDisable(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	other := openTestService(t, dsn, gw, tenants)
	u := mustCreate(t, other, NewUser{Name: "qa1"})
	if err := svc.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := other.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if svc.Disabled(u.ID) {
		t.Fatal("the replica's cache learned the disable without a listener")
	}
	if disabled, err := svc.AccountDisabled(t.Context(), u.ID); err != nil || !disabled {
		t.Errorf("AccountDisabled = %t, %v; want true from the table", disabled, err)
	}
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

func TestALateCreationNotifyStillCutsTheSharesOfADisabledOwner(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	gate := trace(t, svc, dsn, &creationNotifyGate{entered: make(chan struct{}), resume: make(chan struct{})})
	go svc.Listen(context.WithValue(t.Context(), listenerKey{}, true))
	clustertest.WaitFor(t, "the listener loads", func() bool { return gate.loads.Load() > 0 })
	owner := mustCreate(t, svc, NewUser{Name: "qa1"})
	<-gate.entered
	share, err := svc.CreateShare(t.Context(), NewShare{Owner: owner.ID, Box: svc.Boxes(owner.ID)[0].ID})
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	live, ok := svc.ShareBySecretSHA256(sum(share.Secret))
	if !ok {
		t.Fatal("the share does not admit a viewer before the disable")
	}
	if _, err := openTestService(t, dsn, gw, tenants).Update(t.Context(), owner.ID, Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	close(gate.resume)
	clustertest.WaitFor(t, "the listener knows the disable", func() bool { u, err := svc.Get(owner.ID); return err == nil && u.Disabled })
	if err := svc.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := svc.syncShares(t.Context()); err != nil {
		t.Fatalf("syncShares: %v", err)
	}
	if _, ok := svc.ShareBySecretSHA256(sum(share.Secret)); ok {
		t.Error("a disabled owner's share admits a new viewer")
	}
	if !closed(live.Cut) {
		t.Error("a disabled owner's open share stream survives the late notify and a full reload")
	}
}

func TestAFullLoadDoesNotBringBackAUserDeletedWhileItRead(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	other := openTestService(t, dsn, gw, tenants)
	owner, err := other.Ensure(t.Context(), "firebase", outsideID)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	token, err := other.RegenerateToken(t.Context(), owner.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	one := &reloadGate{sql: "FROM boxes WHERE owner", entered: make(chan struct{}), resume: make(chan struct{})}
	all := &reloadGate{sql: "FROM boxes ORDER BY", entered: make(chan struct{}), resume: make(chan struct{})}
	trace(t, svc, dsn, multitracer.New(one, all))
	oneDone := make(chan error, 1)
	go func() { _, err := svc.Ensure(t.Context(), "firebase", outsideID); oneDone <- err }()
	<-one.entered
	if err := other.Delete(t.Context(), owner.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	allDone := make(chan error, 1)
	go func() { allDone <- svc.Load(t.Context()) }()
	<-all.entered
	close(one.resume)
	if err := <-oneDone; err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	close(all.resume)
	if err := <-allDone; err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
		t.Error("the full reload read the delete but a stale single-user read brought the deleted user's token back")
	}
}

func TestARevokeAndADisableReachAListenerWhenNoNotifyCanFollowTheWrite(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	go svc.Listen(t.Context())
	other := openTestService(t, dsn, gw, tenants)
	u := mustCreate(t, other, NewUser{Name: "qa1"})
	token, err := other.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	clustertest.WaitFor(t, "the listener knows the token", func() bool { _, ok := svc.indexedToken(sum(token)); return ok })
	tokenLife, life := lifetime(svc, u.ID, sum(token)), lifetime(svc, u.ID, "")
	trace(t, other, dsn, userstest.LateNotifyFails{})
	if err := other.RevokeToken(t.Context(), u.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	clustertest.WaitFor(t, "the listener ends the revoked token's calls", func() bool { return tokenLife.Err() != nil })
	if _, err := other.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	clustertest.WaitFor(t, "the listener ends the disabled user's lifetime", func() bool { return life.Err() != nil })
}

func TestEveryUsersWriteReachesAListenerWhenNoNotifyCanFollowIt(t *testing.T) {
	tests := []struct {
		name  string
		write func(t *testing.T, a *Service, u User, in signIns) string
		seen  func(b *Service, u User, in signIns, got string) bool
	}{
		{
			name: "box add",
			write: func(t *testing.T, a *Service, u User, _ signIns) string {
				b, err := a.AddBox(t.Context(), Box{ID: "cb-agent", Owner: u.ID, Name: "agent", Agent: true})
				if err != nil {
					t.Fatalf("AddBox: %v", err)
				}
				return b.ID
			},
			seen: func(b *Service, u User, _ signIns, got string) bool { return b.HasBox(u.ID, got) },
		},
		{
			name: "box rename",
			write: func(t *testing.T, a *Service, u User, _ signIns) string {
				if _, err := a.RenameBox(t.Context(), u.ID, firstBoxID(u.ID), "renamed"); err != nil {
					t.Fatalf("RenameBox: %v", err)
				}
				return ""
			},
			seen: func(b *Service, u User, _ signIns, _ string) bool {
				boxes := b.Boxes(u.ID)
				return len(boxes) == 1 && boxes[0].Name == "renamed"
			},
		},
		{
			name: "device issue",
			write: func(t *testing.T, a *Service, u User, _ signIns) string {
				token, err := a.IssueDevice(t.Context(), u.ID, "app", "phone")
				if err != nil {
					t.Fatalf("IssueDevice: %v", err)
				}
				return token
			},
			seen: func(b *Service, _ User, _ signIns, got string) bool { _, ok := b.indexedToken(sum(got)); return ok },
		},
		{
			name: "device revoke",
			write: func(t *testing.T, a *Service, u User, in signIns) string {
				if err := a.RevokeDevice(t.Context(), u.ID, in.deviceID); err != nil {
					t.Fatalf("RevokeDevice: %v", err)
				}
				return ""
			},
			seen: func(b *Service, _ User, in signIns, _ string) bool {
				_, ok := b.indexedToken(sum(in.device))
				return !ok
			},
		},
		{
			name: "user delete",
			write: func(t *testing.T, a *Service, u User, _ signIns) string {
				if err := a.Delete(t.Context(), u.ID); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				return ""
			},
			seen: func(b *Service, u User, _ signIns, _ string) bool {
				_, err := b.Get(u.ID)
				return errors.Is(err, ErrNotFound)
			},
		},
		{
			name: "user disable",
			write: func(t *testing.T, a *Service, u User, _ signIns) string {
				if _, err := a.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil {
					t.Fatalf("Update: %v", err)
				}
				return ""
			},
			seen: func(b *Service, u User, _ signIns, _ string) bool {
				got, err := b.Get(u.ID)
				return err == nil && got.Disabled
			},
		},
		{
			name: "token set",
			write: func(t *testing.T, a *Service, u User, _ signIns) string {
				token, err := a.RegenerateToken(t.Context(), u.ID)
				if err != nil {
					t.Fatalf("RegenerateToken: %v", err)
				}
				return token
			},
			seen: func(b *Service, _ User, _ signIns, got string) bool { _, ok := b.indexedToken(sum(got)); return ok },
		},
		{
			name: "token revoke",
			write: func(t *testing.T, a *Service, u User, _ signIns) string {
				if err := a.RevokeToken(t.Context(), u.ID); err != nil {
					t.Fatalf("RevokeToken: %v", err)
				}
				return ""
			},
			seen: func(b *Service, _ User, in signIns, _ string) bool {
				_, ok := b.indexedToken(sum(in.token))
				return !ok
			},
		},
		{
			name: "sign out everywhere",
			write: func(t *testing.T, a *Service, u User, _ signIns) string {
				if err := a.SignOut(t.Context(), u.ID); err != nil {
					t.Fatalf("SignOut: %v", err)
				}
				return ""
			},
			seen: func(b *Service, _ User, in signIns, _ string) bool {
				_, token := b.indexedToken(sum(in.token))
				_, device := b.indexedToken(sum(in.device))
				return !token && !device
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, gw, tenants, dsn := newTestService(t)
			go b.Listen(t.Context())
			a := openTestService(t, dsn, gw, tenants)
			u := mustCreate(t, a, NewUser{Name: "qa1"})
			token, err := a.RegenerateToken(t.Context(), u.ID)
			if err != nil {
				t.Fatalf("RegenerateToken: %v", err)
			}
			device, err := a.IssueDevice(t.Context(), u.ID, "app", "laptop")
			if err != nil {
				t.Fatalf("IssueDevice: %v", err)
			}
			in := signIns{token: token, device: device, deviceID: a.TokenDevice(sum(device))}
			clustertest.WaitFor(t, "the listener knows the sign-ins", func() bool {
				_, tokenKnown := b.indexedToken(sum(token))
				_, deviceKnown := b.indexedToken(sum(device))
				return tokenKnown && deviceKnown
			})
			trace(t, a, dsn, userstest.LateNotifyFails{})
			got := tt.write(t, a, u, in)
			clustertest.WaitFor(t, "the listener sees the "+tt.name, func() bool { return tt.seen(b, u, in, got) })
		})
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

type listenerKey struct{}

type creationNotifyGate struct {
	entered chan struct{}
	resume  chan struct{}
	blocked atomic.Bool
	loads   atomic.Int64
}

func (g *creationNotifyGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if ctx.Value(listenerKey{}) != true {
		return ctx
	}
	if strings.Contains(data.SQL, "FROM users ORDER BY") {
		g.loads.Add(1)
	}
	if strings.Contains(data.SQL, "FROM users WHERE id =") && g.blocked.CompareAndSwap(false, true) {
		close(g.entered)
		<-g.resume
	}
	return ctx
}

func (*creationNotifyGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type signIns struct {
	token, device, deviceID string
}
