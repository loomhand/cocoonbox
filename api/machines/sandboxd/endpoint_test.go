package sandboxd

import (
	"maps"
	"testing"
)

func TestOwnerMoveChangesTheEndpointCacheKey(t *testing.T) {
	first, next := newFakeSandboxd(t), newFakeSandboxd(t)
	b := newTestBackend(t, first)
	claimRunning(t, b)
	before, err := b.Address(t.Context(), boxID)
	if err != nil {
		t.Fatal(err)
	}
	first.mu.Lock()
	maps.Copy(next.sandboxes, first.sandboxes)
	relays := len(first.relays)
	first.mu.Unlock()
	b.mu.Lock()
	b.entries[boxID].ownerAddr = next.srv.URL
	b.mu.Unlock()
	after, err := b.Address(t.Context(), boxID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Key == before.Key {
		t.Fatal("the moved sandbox kept its old connection cache key")
	}
	conn, err := after.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	first.mu.Lock()
	defer first.mu.Unlock()
	if next.lastRelay() == "" || len(first.relays) != relays {
		t.Fatal("the new endpoint did not dial the new owner")
	}
}
