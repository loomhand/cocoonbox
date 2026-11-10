package users

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/users/userstest"
)

const seededUsers = 20000

func TestIssueDeviceReturnsWithinASecondWhileOtherUsersKeepWriting(t *testing.T) {
	dsn := clustertest.EmptyDSN(t)
	gw, tenants := userstest.NewGW(), userstest.NewTenants()
	a := openTestService(t, dsn, gw, tenants)
	seedUsers(t, dsn, seededUsers)
	b := openTestService(t, dsn, gw, tenants)
	if err := a.load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	go a.Listen(t.Context())
	go b.Listen(t.Context())
	u := mustCreate(t, a, NewUser{Name: "alice"})
	clustertest.WaitFor(t, "the other api knows the user", func() bool { _, err := b.Get(u.ID); return err == nil })
	ctx, stop := context.WithCancel(t.Context())
	var renames atomic.Int64
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := w; ctx.Err() == nil; i += 8 {
				owner := fmt.Sprintf("seed%d", i%seededUsers+1)
				if _, err := b.RenameBox(ctx, owner, "cb-"+owner, fmt.Sprintf("n%d", i)); err == nil {
					renames.Add(1)
				}
			}
		})
	}
	defer func() { stop(); wg.Wait() }()
	clustertest.WaitFor(t, "the other api writes", func() bool { return renames.Load() > 20 })
	var tokens []string
	for range 3 {
		start := time.Now()
		token, err := a.IssueDevice(t.Context(), u.ID, "app", "phone")
		if took := time.Since(start); err != nil || took > time.Second {
			t.Fatalf("IssueDevice = %v after %s while another api renames boxes, want a token within 1s", err, took)
		}
		if owner, ok := a.ByTokenSHA256(t.Context(), sum(token)); !ok || owner != u.ID {
			t.Fatalf("the issuing api resolves the new token to %q, %t", owner, ok)
		}
		tokens = append(tokens, token)
	}
	stop()
	wg.Wait()
	clustertest.WaitFor(t, "the other api resolves every new token", func() bool {
		for _, token := range tokens {
			if _, ok := b.indexedToken(sum(token)); !ok {
				return false
			}
		}
		return true
	})
}

func TestALoadRacingASingleUserReadKeepsTheNewerUser(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	other := openTestService(t, dsn, gw, tenants)
	u := mustCreate(t, other, NewUser{Name: "qa1"})
	if err := svc.SyncUser(t.Context(), u.ID); err != nil {
		t.Fatalf("SyncUser: %v", err)
	}
	gate := gateQuery(t, svc, dsn, "FROM boxes ORDER BY")
	done := make(chan error, 1)
	go func() { done <- svc.load(t.Context()) }()
	<-gate.entered
	if _, err := other.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(3)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := svc.SyncUser(t.Context(), u.ID); err != nil {
		t.Fatalf("SyncUser: %v", err)
	}
	close(gate.resume)
	if err := <-done; err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, err := svc.Get(u.ID); err != nil || got.MaxBoxesCap != 3 {
		t.Errorf("after the Load that began first: max boxes cap %d, %v; want 3 from the newer single-user read", got.MaxBoxesCap, err)
	}
}

func TestALoadRacingADeviceIssueKeepsTheNewToken(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	gate := gateQuery(t, svc, dsn, "FROM boxes ORDER BY")
	done := make(chan error, 1)
	go func() { done <- svc.load(t.Context()) }()
	<-gate.entered
	token, err := svc.IssueDevice(t.Context(), u.ID, "app", "phone")
	close(gate.resume)
	if err != nil {
		t.Fatalf("IssueDevice: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("load: %v", err)
	}
	if owner, ok := svc.indexedToken(sum(token)); !ok || owner.user != u.ID {
		t.Errorf("after the Load that began first the index maps the new device token to %+v, %t; want the user", owner, ok)
	}
}

func TestAnEndedTokenStaysEndedWhileAStaleSingleUserReadInstalls(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	token, err := svc.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	gate := gateQuery(t, svc, dsn, "FROM user_tokens WHERE user_id = $1 ORDER BY")
	synced := make(chan error, 1)
	go func() { synced <- svc.SyncUser(t.Context(), u.ID) }()
	<-gate.entered
	ended := make(chan struct{})
	go func() {
		svc.EndCredential(t.Context(), sum(token))
		close(ended)
	}()
	clustertest.WaitFor(t, "the end runs or waits for the read", func() bool { return closed(ended) || waitingOnLock("(*Service).endLocally") })
	close(gate.resume)
	if err := <-synced; err != nil {
		t.Fatalf("SyncUser: %v", err)
	}
	<-ended
	if _, ok := svc.indexedToken(sum(token)); ok {
		t.Error("a single-user read that began before the end brought the ended token back")
	}
}

func TestASingleUserReadThatBeganBeforeALoadDoesNotUndoIt(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	other := openTestService(t, dsn, gw, tenants)
	u := mustCreate(t, other, NewUser{Name: "qa1"})
	token, err := other.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	gate := gateQuery(t, svc, dsn, "FROM user_tokens WHERE user_id = $1 ORDER BY")
	done := make(chan error, 1)
	go func() { done <- svc.SyncUser(t.Context(), u.ID) }()
	<-gate.entered
	if err := other.RevokeToken(t.Context(), u.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if err := svc.load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	close(gate.resume)
	if err := <-done; err != nil {
		t.Fatalf("SyncUser: %v", err)
	}
	if _, ok := svc.indexedToken(sum(token)); ok {
		t.Error("a single-user read that began before the Load brought the revoked token back")
	}
}

func TestASingleUserReadRacingARevokeLeavesTheTokenRevoked(t *testing.T) {
	b, gw, tenants, dsn := newTestService(t)
	a := openTestService(t, dsn, gw, tenants)
	u := mustCreate(t, a, NewUser{Name: "qa1"})
	token, err := a.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	gate := gateQuery(t, b, dsn, "FROM user_tokens WHERE user_id = $1 ORDER BY")
	go b.Listen(t.Context())
	clustertest.WaitFor(t, "the listener knows the token", func() bool { _, ok := b.indexedToken(sum(token)); return ok })
	tokenLife := lifetime(b, u.ID, sum(token))
	if _, err := a.RenameBox(t.Context(), u.ID, firstBoxID(u.ID), "renamed"); err != nil {
		t.Fatalf("RenameBox: %v", err)
	}
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener read no single user for the rename")
	}
	if err := a.RevokeToken(t.Context(), u.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	close(gate.resume)
	clustertest.WaitFor(t, "the listener drops the revoked token and ends its calls", func() bool {
		_, ok := b.indexedToken(sum(token))
		return !ok && tokenLife.Err() != nil
	})
	if lifetime(b, u.ID, sum(token)).Err() == nil {
		t.Error("the revoked token opened a new lifetime on the listener")
	}
}

func TestADisableOrDeleteOnOneApiEndsTheUsersCallsSharesAndGrantsOnAnother(t *testing.T) {
	for name, del := range map[string]bool{"disable": false, "delete": true} {
		t.Run(name, func(t *testing.T) {
			b, gw, tenants, dsn := newTestService(t)
			var asked atomic.Bool
			b.KeepGrants(func(credential string) bool { return credential == "grant-1" }, func(_ context.Context, ids []string) (map[string]bool, error) {
				asked.Store(len(ids) == 1 && ids[0] == "grant-1")
				return map[string]bool{}, nil
			})
			loads := trace(t, b, dsn, &queryCounter{sql: "FROM users ORDER BY"})
			go b.Listen(t.Context())
			clustertest.WaitFor(t, "the listener loads once", func() bool { return loads.n.Load() == 1 })
			a := openTestService(t, dsn, gw, tenants)
			u := mustCreate(t, a, NewUser{Name: "qa1"})
			share, err := a.CreateShare(t.Context(), NewShare{Owner: u.ID, Box: firstBoxID(u.ID)})
			if err != nil {
				t.Fatalf("CreateShare: %v", err)
			}
			clustertest.WaitFor(t, "the listener knows the user and the share", func() bool {
				_, err := b.Get(u.ID)
				_, ok := b.ShareBySecretSHA256(sum(share.Secret))
				return err == nil && ok
			})
			live, _ := b.ShareBySecretSHA256(sum(share.Secret))
			userLife, grantLife := lifetime(b, u.ID, ""), lifetime(b, u.ID, "grant-1")
			if del {
				err = a.Delete(t.Context(), u.ID)
			} else {
				_, err = a.Update(t.Context(), u.ID, Settings{Disabled: new(true)})
			}
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			clustertest.WaitFor(t, "the listener ends the user's calls, share streams and grants", func() bool {
				return userLife.Err() != nil && grantLife.Err() != nil && closed(live.Cut) && asked.Load()
			})
			b.mu.RLock()
			_, tomb := b.tombs["grant-1"]
			b.mu.RUnlock()
			if !tomb {
				t.Error("the user's ended grant can bind again")
			}
			if n := loads.n.Load(); n != 1 {
				t.Errorf("the listener reloaded fully %d times, want none", n-1)
			}
		})
	}
}

func TestSyncUserReadsAnotherApisNewBoxWithoutAFullReload(t *testing.T) {
	b, gw, tenants, dsn := newTestService(t)
	a := openTestService(t, dsn, gw, tenants)
	u := mustCreate(t, a, NewUser{Name: "qa1"})
	loads := trace(t, b, dsn, &queryCounter{sql: "FROM users ORDER BY"})
	if _, err := a.AddBox(t.Context(), Box{ID: "cb-agent", Owner: u.ID, Name: "agent", Agent: true}); err != nil {
		t.Fatalf("AddBox: %v", err)
	}
	if err := b.SyncUser(t.Context(), u.ID); err != nil {
		t.Fatalf("SyncUser: %v", err)
	}
	if !b.HasBox(u.ID, "cb-agent") || len(b.Boxes(u.ID)) != 2 {
		t.Errorf("after SyncUser the other api holds boxes %+v, want main and agent", b.Boxes(u.ID))
	}
	if n := loads.n.Load(); n != 0 {
		t.Errorf("SyncUser reloaded fully %d times, want none", n)
	}
}

func seedUsers(t *testing.T, dsn string, n int) {
	t.Helper()
	for _, sql := range []string{
		`INSERT INTO users (id, name, max_boxes, max_boxes_cap, tenant_token_sealed) SELECT 'seed' || g, 'seed' || g, 1, 1, '\x00'::bytea FROM generate_series(1, $1) g`,
		`INSERT INTO boxes (id, owner, name) SELECT 'cb-seed' || g, 'seed' || g, 'main' FROM generate_series(1, $1) g`,
		`INSERT INTO user_tokens (id, user_id, sha256, client, label) SELECT 'dev_seed' || g, 'seed' || g, md5(g::text) || md5((g + 1)::text), 'app', 'seed' FROM generate_series(1, $1) g`,
	} {
		if err := userstest.Exec(t, dsn, sql, n); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func waitingOnLock(in string) bool {
	buf := make([]byte, 1<<20)
	for g := range strings.SplitSeq(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		if strings.Contains(g, "[sync.Mutex.Lock") && strings.Contains(g, in) {
			return true
		}
	}
	return false
}
