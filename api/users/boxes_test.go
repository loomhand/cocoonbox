package users

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"sync"
	"testing"
)

func TestEveryUserStartsWithOneBox(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u, err := svc.Create(t.Context(), NewUser{Name: "qa1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
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

func TestTheMigrationGivesEveryUserItsOldBox(t *testing.T) {
	if firstBoxID("alice") != "cb-2bd806c97f0e" {
		t.Fatalf("firstBoxID(alice) = %s, want the box id boxes.BoxID gives", firstBoxID("alice"))
	}
	svc, gw, tenants, dsn := newTestService(t)
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1", ID: "test"})
	if err := exec(t, dsn, "DROP TABLE boxes; DELETE FROM schema_migrations WHERE version = 4"); err != nil {
		t.Fatalf("undo migration 4: %v", err)
	}
	got := openTestService(t, dsn, gw, tenants).Boxes(u.ID)
	if len(got) != 1 || got[0].ID != firstBoxID("test") || got[0].Name != FirstBoxName {
		t.Fatalf("Boxes after migration = %+v, want %s", got, firstBoxID("test"))
	}
}

func TestAddBoxKeepsTheQuotaAndUniqueNames(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
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
	var mu sync.Mutex
	var quota int
	for i := range 6 {
		wg.Go(func() {
			_, err := svc.AddBox(t.Context(), Box{ID: "cb-c" + string(rune('0'+i)), Owner: u.ID, Name: "box" + string(rune('0'+i))})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrQuota):
				quota++
			case err != nil:
				t.Errorf("AddBox: %v", err)
			}
		})
	}
	wg.Wait()
	if n := len(svc.Boxes(u.ID)); n != 4 || quota != 3 {
		t.Errorf("after 6 parallel adds: %d boxes, %d refused, want 4 and 3", n, quota)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxes: new(2)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("lowering max boxes under the box count = %v, want ErrInvalid", err)
	}
	list := svc.Boxes(u.ID)
	if list[0].Name != FirstBoxName || !slices.IsSortedFunc(list, compareBoxes) {
		t.Errorf("Boxes = %+v, want oldest first", list)
	}
}

func TestRenameAndRemoveBox(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	other, _ := svc.Create(t.Context(), NewUser{Name: "qa2"})
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

	kept, keptSecret, _ := svc.CreateShare(t.Context(), NewShare{Owner: u.ID, Box: firstBoxID(u.ID)})
	gone, goneSecret, _ := svc.CreateShare(t.Context(), NewShare{Owner: u.ID, Box: test.ID})
	live, _ := svc.ShareBySecretSHA256(sum(goneSecret))
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
	if _, ok := svc.ShareBySecretSHA256(sum(goneSecret)); ok {
		t.Error("the removed box's share still admits")
	}
	if _, ok := svc.ShareBySecretSHA256(sum(keptSecret)); !ok {
		t.Errorf("share %s of the other box ended", kept.ID)
	}
	var revoked bool
	if err := query(t, dsn, "SELECT revoked_at IS NOT NULL FROM shares WHERE id = $1", gone.ID).Scan(&revoked); err != nil || !revoked {
		t.Errorf("share row revoked = %t, %v", revoked, err)
	}
	reopened := openTestService(t, dsn, gw, tenants)
	if got := reopened.Boxes(u.ID); len(got) != 1 || got[0].ID != firstBoxID(u.ID) {
		t.Errorf("Boxes after reopen = %+v", got)
	}
	if err := svc.Delete(t.Context(), u.ID); err != nil || len(svc.Boxes(u.ID)) != 0 {
		t.Errorf("Delete = %v, boxes left %+v", err, svc.Boxes(u.ID))
	}
}

func firstBoxID(user string) string {
	s := sha256.Sum256([]byte(user))
	return "cb-" + hex.EncodeToString(s[:])[:12]
}

func TestTheRunningLimitIsTheUsersOwnOrTheDefault(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
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
	if err := query(t, dsn, "SELECT max_running FROM users WHERE id = $1", u.ID).Scan(&stored); err != nil || stored != nil {
		t.Errorf("max_running after inherit = %v, %v, want NULL", stored, err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{MaxRunning: new(-1)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a negative running limit = %v, want ErrInvalid", err)
	}
}
