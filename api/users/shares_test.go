package users

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CMGS/cocoonbox/api/auth"
)

func TestShareLifecycle(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	owner, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	guest, _ := svc.Create(t.Context(), NewUser{Name: "qa2"})
	var ended []string
	svc.OnShareEnd(func(_ context.Context, sh Share) { ended = append(ended, sh.ID) })

	sh, secret, err := svc.CreateShare(t.Context(), NewShare{Owner: owner.ID, Box: "cb-1", UserNames: []string{"qa2", "qa2"}})
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if !strings.HasPrefix(secret, auth.SecretPrefix) || len(secret) != len(auth.SecretPrefix)+32 || !strings.HasPrefix(sh.ID, shareIDPrefix) {
		t.Errorf("share %s has secret of %d bytes", sh.ID, len(secret))
	}
	if ttl := time.Until(sh.ExpiresAt); ttl < DefaultShareTTL-time.Minute || ttl > DefaultShareTTL {
		t.Errorf("default expiry in %s", ttl)
	}
	if len(sh.Users) != 1 || sh.Users[0] != (ShareUser{ID: guest.ID, Name: "qa2"}) || sh.Write {
		t.Errorf("share = %+v", sh)
	}
	var stored string
	if err := query(t, dsn, "SELECT secret_sha256 FROM shares WHERE id = $1", sh.ID).Scan(&stored); err != nil || stored != sum(secret) {
		t.Errorf("stored %q, %v; want only the secret's sha256", stored, err)
	}
	got, ok := svc.ShareBySecretSHA256(sum(secret))
	if !ok || got.Owner != owner.ID || got.Box != "cb-1" || !slices.Equal(got.Users, []string{guest.ID}) {
		t.Fatalf("ShareBySecretSHA256 = %+v, %t", got, ok)
	}
	if _, _, err := svc.CreateShare(t.Context(), NewShare{Owner: owner.ID, Box: "cb-1"}); !errors.Is(err, ErrExists) {
		t.Errorf("a second live share of the box: %v, want ErrExists", err)
	}
	for _, ns := range []NewShare{{Owner: owner.ID, Box: "cb-2", TTL: MaxShareTTL + time.Hour}, {Owner: owner.ID, Box: "cb-2", UserNames: []string{"nobody"}}} {
		if _, _, err := svc.CreateShare(t.Context(), ns); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateShare(%+v) = %v, want ErrInvalid", ns, err)
		}
	}

	moded := got.Moded
	if _, err := svc.SetShareWrite(t.Context(), guest.ID, sh.ID, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetShareWrite by another user: %v, want ErrNotFound", err)
	}
	if w, err := svc.SetShareWrite(t.Context(), owner.ID, sh.ID, true); err != nil || !w.Write {
		t.Fatalf("SetShareWrite = %+v, %v", w, err)
	}
	if !closed(moded) || closed(got.Cut) {
		t.Error("a write switch must close Moded and leave Cut open")
	}

	reopened := openTestService(t, dsn, gw, tenants)
	if again, ok := reopened.ShareBySecretSHA256(sum(secret)); !ok || !again.Write {
		t.Errorf("after reopen: %+v, %t", again, ok)
	}

	_, fresh, err := svc.RegenerateShare(t.Context(), owner.ID, sh.ID)
	if err != nil || fresh == secret {
		t.Fatalf("RegenerateShare: %v", err)
	}
	if _, ok := svc.ShareBySecretSHA256(sum(secret)); ok || !closed(got.Cut) {
		t.Error("the old secret still works, or its streams were not cut")
	}
	now, ok := svc.ShareBySecretSHA256(sum(fresh))
	if !ok || now.ID != sh.ID {
		t.Fatalf("the new secret resolves to %+v, %t", now, ok)
	}

	if err := svc.RevokeShare(t.Context(), guest.ID, sh.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("RevokeShare by another user: %v, want ErrNotFound", err)
	}
	if err := svc.RevokeShare(t.Context(), owner.ID, sh.ID); err != nil {
		t.Fatalf("RevokeShare: %v", err)
	}
	if _, ok := svc.ShareBySecretSHA256(sum(fresh)); ok || !closed(now.Cut) || len(svc.Shares(owner.ID)) != 0 {
		t.Error("a revoked share still works or is listed")
	}
	if !slices.Equal(ended, []string{sh.ID, sh.ID}) {
		t.Errorf("end hooks ran for %v, want the regenerate and the revoke", ended)
	}
	if _, _, err := svc.CreateShare(t.Context(), NewShare{Owner: owner.ID, Box: "cb-1"}); err != nil {
		t.Errorf("CreateShare after a revoke: %v", err)
	}
}

func TestShareExpiresAndDiesWithItsOwner(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	owner, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	expired := make(chan string, 1)
	svc.OnShareEnd(func(_ context.Context, sh Share) { expired <- sh.ID })

	sh, secret, err := svc.CreateShare(t.Context(), NewShare{Owner: owner.ID, Box: "cb-1", TTL: minShareTTL})
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	live, _ := svc.ShareBySecretSHA256(sum(secret))
	svc.mu.Lock()
	svc.shares[sh.ID].timer.Reset(10 * time.Millisecond)
	svc.mu.Unlock()
	select {
	case id := <-expired:
		if id != sh.ID {
			t.Errorf("expired %s, want %s", id, sh.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the share did not expire")
	}
	if _, ok := svc.ShareBySecretSHA256(sum(secret)); ok || !closed(live.Cut) {
		t.Error("an expired share still works, or its streams were not cut")
	}
	if err := exec(t, dsn, "UPDATE shares SET expires_at = now() - interval '1 second' WHERE id = $1", sh.ID); err != nil {
		t.Fatalf("expire row: %v", err)
	}
	if _, _, err := svc.CreateShare(t.Context(), NewShare{Owner: owner.ID, Box: "cb-1"}); err != nil {
		t.Fatalf("CreateShare over an expired one: %v", err)
	}

	again, secret, _ := svc.CreateShare(t.Context(), NewShare{Owner: owner.ID, Box: "cb-2"})
	live, _ = svc.ShareBySecretSHA256(sum(secret))
	disabled := true
	if _, err := svc.Update(t.Context(), owner.ID, Settings{Disabled: &disabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok := svc.ShareBySecretSHA256(sum(secret)); ok || !closed(live.Cut) {
		t.Error("the share of a disabled owner still works, or its streams were not cut")
	}
	if err := svc.Delete(t.Context(), owner.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var rows int
	if err := query(t, dsn, "SELECT count(*) FROM shares WHERE id = $1", again.ID).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d share rows outlive their owner, %v", rows, err)
	}
	if svc.mu.RLock(); len(svc.shares) != 0 {
		t.Errorf("the index keeps %d shares of a deleted owner", len(svc.shares))
	}
	svc.mu.RUnlock()
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
