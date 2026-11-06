package flows

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
)

func TestSharedLibrariesKeepBothConcurrentReplacements(t *testing.T) {
	shared, dsn := clustertest.OpenDSN(t)
	other, err := cluster.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	st, seed := newLibrary()
	for _, name := range []string{"a.json", "b.json"} {
		if _, err := seed.Put(t.Context(), "alice", name, []byte("old"), Origin{}); err != nil {
			t.Fatal(err)
		}
	}
	gate := &libraryReadGate{memStore: st, entered: make(chan struct{}), resume: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(gate.resume) })
	defer resume()
	first, second := NewLibrary(gate, fixedKeys{}), NewLibrary(st, fixedKeys{})
	first.EnableShared(shared)
	second.EnableShared(other)
	done := make(chan error, 2)
	go func() { _, err := first.Put(t.Context(), "alice", "a.json", []byte("new a"), Origin{}); done <- err }()
	<-gate.entered
	go func() { _, err := second.Put(t.Context(), "alice", "b.json", []byte("new b"), Origin{}); done <- err }()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	clustertest.WaitFor(t, "the second replica waits for the library lock", func() bool {
		var waiting bool
		err := conn.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database()))").Scan(&waiting)
		return err == nil && waiting
	})
	resume()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{"a.json": "new a", "b.json": "new b"} {
		if _, content, err := second.Get(t.Context(), "alice", name); err != nil || string(content) != want {
			t.Errorf("Get(%s) = %q, %v; want %q", name, content, err, want)
		}
	}
	if rev, _, err := second.List(t.Context(), "alice"); err != nil || rev != 4 {
		t.Errorf("revision = %d, %v; want 4", rev, err)
	}
}

type libraryReadGate struct {
	*memStore
	entered chan struct{}
	resume  chan struct{}
	blocked atomic.Bool
}

func (g *libraryReadGate) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := g.memStore.Get(ctx, key)
	if key == indexKey("alice") && g.blocked.CompareAndSwap(false, true) {
		close(g.entered)
		<-g.resume
	}
	return data, err
}
