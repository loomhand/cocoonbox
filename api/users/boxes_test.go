package users

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/users/userstest"
	"github.com/loomhand/cocoonbox/api/utils"
)

func TestEveryUserStartsWithOneBox(t *testing.T) {
	if firstBoxID("alice") != "cb-2bd806c97f0e" {
		t.Fatalf("firstBoxID(alice) = %s, want the box id boxes.BoxID gives", firstBoxID("alice"))
	}
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	if got := svc.Boxes(u.ID); len(got) != 1 || got[0].ID != firstBoxID(u.ID) || got[0].Name != FirstBoxName || got[0].Owner != u.ID {
		t.Fatalf("Boxes = %+v", got)
	}
	if !svc.HasBox(u.ID, firstBoxID(u.ID)) || svc.HasBox("u-other", firstBoxID(u.ID)) {
		t.Error("HasBox does not follow ownership")
	}
	if got := openTestService(t, dsn, gw, tenants).Boxes(u.ID); len(got) != 1 || got[0].ID != firstBoxID(u.ID) {
		t.Errorf("Boxes after reopen = %+v", got)
	}
}

func TestTheConfiguredMaxBoxesHoldsForEveryUserWithoutItsOwn(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	big := mustCreate(t, svc, NewUser{Name: "qa2"})
	small := mustCreate(t, svc, NewUser{Name: "qa3"})
	capped := mustCreate(t, svc, NewUser{Name: "qa4"})
	if _, err := svc.Update(t.Context(), big.ID, Settings{MaxBoxesCap: new(5), MaxBoxes: new(5)}); err != nil {
		t.Fatalf("raise qa2's max boxes: %v", err)
	}
	if _, err := svc.Update(t.Context(), small.ID, Settings{MaxBoxes: new(1)}); err != nil {
		t.Fatalf("pin qa3 to one box: %v", err)
	}
	if _, err := svc.Update(t.Context(), capped.ID, Settings{MaxBoxesCap: new(2)}); err != nil {
		t.Fatalf("cap qa4 at two boxes: %v", err)
	}
	opts := Options{DSN: dsn, MasterKeyFile: userstest.MasterKeyFile(t), GW: gw, Tenants: tenants, TenantClaims: true, FirstBoxID: firstBoxID, EnsureLimit: 4, PasswordCost: bcrypt.MinCost, MaxBoxes: 3}
	raised, err := Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("Open with max boxes 3: %v", err)
	}
	t.Cleanup(raised.Close)
	if got, _ := raised.Get(u.ID); got.MaxBoxes != 3 || got.MaxBoxesCap != 3 {
		t.Errorf("qa1 = %d of at most %d, want the configured 3 of 3", got.MaxBoxes, got.MaxBoxesCap)
	}
	if got, _ := raised.Get(capped.ID); got.MaxBoxes != 2 || got.MaxBoxesCap != 2 {
		t.Errorf("qa4 = %d of at most %d, want its own cap of 2 to bound the configured 3", got.MaxBoxes, got.MaxBoxesCap)
	}
	if _, err := raised.AddBox(t.Context(), Box{ID: "cb-capped", Owner: capped.ID, Name: "b"}); err != nil {
		t.Fatalf("AddBox within qa4's cap: %v", err)
	}
	if _, err := raised.AddBox(t.Context(), Box{ID: "cb-capped2", Owner: capped.ID, Name: "c"}); !errors.Is(err, ErrQuota) {
		t.Errorf("AddBox past qa4's cap of 2 = %v, want ErrQuota", err)
	}
	for _, name := range []string{"b", "c"} {
		if _, err := raised.AddBox(t.Context(), Box{ID: "cb-" + name, Owner: u.ID, Name: name}); err != nil {
			t.Fatalf("AddBox %s within max boxes 3: %v", name, err)
		}
	}
	if _, err := raised.AddBox(t.Context(), Box{ID: "cb-d", Owner: u.ID, Name: "d"}); !errors.Is(err, ErrQuota) {
		t.Errorf("AddBox past max boxes 3 = %v, want ErrQuota", err)
	}
	if _, err := raised.AddBox(t.Context(), Box{ID: "cb-small", Owner: small.ID, Name: "b"}); !errors.Is(err, ErrQuota) {
		t.Errorf("AddBox past qa3's own 1 = %v, want ErrQuota", err)
	}
	if _, err := raised.reconcileTenants(t.Context()); err != nil {
		t.Fatalf("reconcileTenants: %v", err)
	}
	for id, want := range map[string]int{u.ID: 4, big.ID: 6, small.ID: 2, capped.ID: 3} {
		if got := tenants.Max[id]; got != want {
			t.Errorf("tenant cap of %s = %d, want %d", id, got, want)
		}
	}
	opts.MaxBoxes = maxBoxesCap + 1
	if _, err := Open(t.Context(), opts); err == nil {
		t.Error("Open with max boxes past the cap succeeded")
	}
}

func TestTheMigrationHandsUsersAtTheCreationValuesToTheConfig(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	rows := map[string][2]int{"qa1": {1, 1}, "qa2": {4, 4}, "qa3": {1, 3}}
	ids := map[string]string{}
	for name := range rows {
		ids[name] = mustCreate(t, svc, NewUser{Name: name}).ID
	}
	svc.Close()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	for name, row := range rows {
		if _, err := conn.Exec(t.Context(), "UPDATE users SET max_boxes = $2, max_boxes_cap = $3 WHERE id = $1", ids[name], row[0], row[1]); err != nil {
			t.Fatalf("set %s's row: %v", name, err)
		}
	}
	if _, err := conn.Exec(t.Context(), "DELETE FROM schema_migrations WHERE version = 16"); err != nil {
		t.Fatalf("undo migration 16: %v", err)
	}
	openTestService(t, dsn, gw, tenants)
	want := map[string][2]int{"qa1": {0, 0}, "qa2": {4, 4}, "qa3": {1, 3}}
	for name, w := range want {
		var got [2]int
		if err := conn.QueryRow(t.Context(), "SELECT max_boxes, max_boxes_cap FROM users WHERE id = $1", ids[name]).Scan(&got[0], &got[1]); err != nil || got != w {
			t.Errorf("%s after migration = %v, %v; want %v", name, got, err, w)
		}
	}
}

func TestAddBoxKeepsTheQuotaAndUniqueNames(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	if _, err := svc.AddBox(t.Context(), Box{ID: "cb-a", Owner: u.ID, Name: "test"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("AddBox past max boxes = %v, want ErrQuota", err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(4), MaxBoxes: new(4)}); err != nil {
		t.Fatalf("raise max boxes: %v", err)
	}
	for _, name := range []string{"", "Test", "cb-x", "a b", "x123456789012345678901234567890123"} {
		if _, err := svc.AddBox(t.Context(), Box{ID: "cb-b", Owner: u.ID, Name: name}); !errors.Is(err, ErrInvalid) {
			t.Errorf("AddBox named %q = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := svc.AddBox(t.Context(), Box{ID: "cb-b", Owner: u.ID, Name: FirstBoxName}); !errors.Is(err, ErrExists) {
		t.Errorf("AddBox with a taken name = %v, want ErrExists", err)
	}
	var wg sync.WaitGroup
	var quota atomic.Int32
	for i := range 6 {
		wg.Go(func() {
			_, err := svc.AddBox(t.Context(), Box{ID: "cb-c" + strconv.Itoa(i), Owner: u.ID, Name: "box" + strconv.Itoa(i)})
			switch {
			case errors.Is(err, ErrQuota):
				quota.Add(1)
			case err != nil:
				t.Errorf("AddBox: %v", err)
			}
		})
	}
	wg.Wait()
	if n := len(svc.Boxes(u.ID)); n != 4 || quota.Load() != 3 {
		t.Errorf("after 6 parallel adds: %d boxes, %d refused, want 4 and 3", n, quota.Load())
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxes: new(2)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("lowering max boxes under the box count = %v, want ErrInvalid", err)
	}
	list := svc.Boxes(u.ID)
	if list[0].Name != FirstBoxName || !slices.IsSortedFunc(list, compareBoxes) {
		t.Errorf("Boxes = %+v, want oldest first", list)
	}
}

func TestAgentBoxesStayOutsideMaxBoxesAndRaiseTheTenantCap(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	for _, id := range []string{"cb-a1", "cb-a2"} {
		if _, err := svc.AddBox(t.Context(), Box{ID: id, Owner: u.ID, Name: id[3:], Agent: true}); err != nil {
			t.Fatalf("AddBox of an agent with max boxes 1 used = %v", err)
		}
	}
	if got := tenants.Max[u.ID]; got != 4 {
		t.Errorf("tenant cap = %d, want max boxes 1 + 2 agent boxes + 1", got)
	}
	if _, err := svc.AddBox(t.Context(), Box{ID: "cb-o", Owner: u.ID, Name: "own"}); !errors.Is(err, ErrQuota) {
		t.Errorf("the owner's AddBox past max boxes = %v, want ErrQuota", err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxes: new(1)}); err != nil {
		t.Errorf("max boxes 1 with one own and two agent boxes = %v", err)
	}
	if got := openTestService(t, dsn, gw, tenants).Boxes(u.ID); len(got) != 3 || !got[1].Agent || got[0].Agent {
		t.Errorf("Boxes after reopen = %+v, want main and two agent boxes", got)
	}
	if err := svc.RemoveBox(t.Context(), u.ID, "cb-a1"); err != nil {
		t.Fatalf("RemoveBox: %v", err)
	}
	if got := tenants.Max[u.ID]; got != 3 {
		t.Errorf("tenant cap after a removal = %d, want 3", got)
	}
}

func TestRenameAndRemoveBox(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	other := mustCreate(t, svc, NewUser{Name: "qa2"})
	_, _ = svc.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(2), MaxBoxes: new(2)})
	test, err := svc.AddBox(t.Context(), Box{ID: "cb-test", Owner: u.ID, Name: "test"})
	if err != nil {
		t.Fatalf("AddBox: %v", err)
	}
	if _, err := svc.RenameBox(t.Context(), u.ID, test.ID, FirstBoxName); !errors.Is(err, ErrExists) {
		t.Errorf("rename onto a taken name = %v, want ErrExists", err)
	}
	if _, err := svc.RenameBox(t.Context(), other.ID, test.ID, "mine"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename of another user's box = %v, want ErrNotFound", err)
	}
	if b, err := svc.RenameBox(t.Context(), u.ID, test.ID, "work"); err != nil || b.Name != "work" {
		t.Fatalf("RenameBox = %+v, %v", b, err)
	}

	kept, _ := svc.CreateShare(t.Context(), NewShare{Owner: u.ID, Box: firstBoxID(u.ID)})
	gone, _ := svc.CreateShare(t.Context(), NewShare{Owner: u.ID, Box: test.ID})
	live, _ := svc.ShareBySecretSHA256(sum(gone.Secret))
	var ended []string
	svc.OnShareEnd(func(_ context.Context, sh Share) { ended = append(ended, sh.ID) })
	if err := svc.RemoveBox(t.Context(), other.ID, test.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveBox by another user = %v, want ErrNotFound", err)
	}
	if err := svc.RemoveBox(t.Context(), u.ID, test.ID); err != nil {
		t.Fatalf("RemoveBox: %v", err)
	}
	if svc.HasBox(u.ID, test.ID) || !closed(live.Cut) || !slices.Equal(ended, []string{gone.ID}) {
		t.Errorf("after RemoveBox: has %t, cut %t, ended %v", svc.HasBox(u.ID, test.ID), closed(live.Cut), ended)
	}
	if _, ok := svc.ShareBySecretSHA256(sum(gone.Secret)); ok {
		t.Error("the removed box's share still admits")
	}
	if _, ok := svc.ShareBySecretSHA256(sum(kept.Secret)); !ok {
		t.Errorf("share %s of the other box ended", kept.ID)
	}
	var revoked, sealed bool
	if err := userstest.Query(t, dsn, "SELECT revoked_at IS NOT NULL, secret_sealed IS NOT NULL FROM shares WHERE id = $1", gone.ID).Scan(&revoked, &sealed); err != nil || !revoked || sealed {
		t.Errorf("share row revoked = %t, keeps its secret = %t, %v", revoked, sealed, err)
	}
	reopened := openTestService(t, dsn, gw, tenants)
	if got := reopened.Boxes(u.ID); len(got) != 1 || got[0].ID != firstBoxID(u.ID) {
		t.Errorf("Boxes after reopen = %+v", got)
	}
	if err := svc.Delete(t.Context(), u.ID); err != nil || len(svc.Boxes(u.ID)) != 0 {
		t.Errorf("Delete = %v, boxes left %+v", err, svc.Boxes(u.ID))
	}
}

func TestTheRunningLimitIsTheUsersOwnOrTheDefault(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "qa1"})
	if u.MaxRunning != 0 || svc.MaxRunning(u.ID) != 0 {
		t.Fatalf("a new user's running limit = %d, want 0 (the default)", svc.MaxRunning(u.ID))
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxRunning: new(2)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a running limit over max boxes = %v, want ErrInvalid", err)
	}
	if got, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(3), MaxBoxes: new(3), MaxRunning: new(3)}); err != nil || got.MaxRunning != 3 {
		t.Fatalf("Update = %+v, %v", got, err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxes: new(2)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("lowering max boxes under the running limit = %v, want ErrInvalid", err)
	}
	if got := openTestService(t, dsn, gw, tenants).MaxRunning(u.ID); got != 3 {
		t.Errorf("running limit after reopen = %d, want 3", got)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxRunning: new(0)}); err != nil || svc.MaxRunning(u.ID) != 0 {
		t.Fatalf("back to the default: %v, limit %d", err, svc.MaxRunning(u.ID))
	}
	var stored *int
	if err := userstest.Query(t, dsn, "SELECT max_running FROM users WHERE id = $1", u.ID).Scan(&stored); err != nil || stored != nil {
		t.Errorf("max_running after inherit = %v, %v, want NULL", stored, err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxRunning: new(-1)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a negative running limit = %v, want ErrInvalid", err)
	}
}

func TestTheUsersAgentBoxCapBoundsEveryGrantAndTheTenantCap(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u, err := svc.Create(t.Context(), NewUser{Name: "qa1", IDP: "github", MaxAgentBoxes: new(1), DailyCapMicros: new(int64(1_000_000))})
	if err != nil || u.IDP != "github" || u.MaxAgentBoxes == nil || *u.MaxAgentBoxes != 1 || gw.Caps[u.ID] != 1_000_000 {
		t.Fatalf("Create = %+v, %v; gw caps %v", u, err, gw.Caps)
	}
	if got := tenants.Max[u.ID]; got != 2 {
		t.Errorf("tenant cap of a new user = %d, want max boxes 1 + 0 agent boxes + 1", got)
	}
	if _, err := svc.AddBox(t.Context(), Box{ID: "cb-a1", Owner: u.ID, Name: "a1", Agent: true}); err != nil {
		t.Fatalf("the first agent box: %v", err)
	}
	if _, err := svc.AddBox(t.Context(), Box{ID: "cb-a2", Owner: u.ID, Name: "a2", Agent: true}); !errors.Is(err, ErrQuota) {
		t.Errorf("a second agent box, from any grant, past max agent boxes 1 = %v, want ErrQuota", err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxAgentBoxes: new(0)}); err != nil {
		t.Fatalf("drop the cap: %v", err)
	}
	if _, err := svc.AddBox(t.Context(), Box{ID: "cb-a2", Owner: u.ID, Name: "a2", Agent: true}); err != nil {
		t.Fatalf("an agent box with no cap: %v", err)
	}
	if got := tenants.Max[u.ID]; got != 4 {
		t.Errorf("tenant cap with two agent boxes and no cap = %d, want 4", got)
	}
	if got, err := svc.Update(t.Context(), u.ID, Settings{MaxAgentBoxes: new(1)}); err != nil || tenants.Max[u.ID] != 3 || *got.MaxAgentBoxes != 1 {
		t.Errorf("cap 1 over two agent boxes = %+v, %v; tenant cap %d, want min(2, 1) counted, 3", got, err, tenants.Max[u.ID])
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxAgentBoxes: new(17)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a cap over the limit = %v, want ErrInvalid", err)
	}
	if again, err := openTestService(t, dsn, gw, tenants).Get(u.ID); err != nil || again.MaxAgentBoxes == nil || *again.MaxAgentBoxes != 1 || again.IDP != "github" {
		t.Errorf("after reopen = %+v, %v", again, err)
	}
}

func TestABoxChangeOfOneApiReachesAnother(t *testing.T) {
	a, gw, tenants, dsn := newTestService(t)
	b := openTestService(t, dsn, gw, tenants)
	go b.Listen(t.Context())
	u := mustCreate(t, a, NewUser{Name: "qa1"})
	if _, err := a.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(2), MaxBoxes: new(2)}); err != nil {
		t.Fatalf("raise max boxes: %v", err)
	}
	if _, err := a.AddBox(t.Context(), Box{ID: "cb-x", Owner: u.ID, Name: "extra"}); err != nil {
		t.Fatalf("AddBox: %v", err)
	}
	clustertest.WaitFor(t, "the other api knows the added box", func() bool { return b.HasBox(u.ID, "cb-x") })
	var changed atomic.Int32
	b.OnBoxes(func(owner string) {
		if owner == u.ID {
			changed.Add(1)
		}
	})
	if _, err := a.RenameBox(t.Context(), u.ID, "cb-x", "renamed"); err != nil {
		t.Fatalf("RenameBox: %v", err)
	}
	clustertest.WaitFor(t, "the other api knows the new name", func() bool {
		return slices.ContainsFunc(b.Boxes(u.ID), func(x Box) bool { return x.ID == "cb-x" && x.Name == "renamed" })
	})
	clustertest.WaitFor(t, "the other api tells its box watchers of the rename", func() bool { return changed.Load() > 0 })
	if err := a.RemoveBox(t.Context(), u.ID, "cb-x"); err != nil {
		t.Fatalf("RemoveBox: %v", err)
	}
	clustertest.WaitFor(t, "the other api drops the removed box", func() bool { return !b.HasBox(u.ID, "cb-x") })
}

func firstBoxID(user string) string {
	return "cb-" + utils.HashHex(user)[:12]
}
