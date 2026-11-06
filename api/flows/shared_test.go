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
	shared, other, dsn := openReplicas(t)
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
	clustertest.WaitFor(t, "the second replica waits for the library lock", advisoryWaiting(t, dsn))
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

func TestSharedGetKeepsItsObjectUntilTheReadFinishes(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "replace", true: "delete"}[remove], func(t *testing.T) {
			shared, other, dsn := openReplicas(t)
			st, seed := newLibrary()
			if _, err := seed.Put(t.Context(), "alice", "a.json", []byte("old"), Origin{}); err != nil {
				t.Fatal(err)
			}
			gate := &libraryReadGate{memStore: st, entered: make(chan struct{}), resume: make(chan struct{})}
			resume := sync.OnceFunc(func() { close(gate.resume) })
			defer resume()
			reader, writer := NewLibrary(gate, fixedKeys{}), NewLibrary(st, fixedKeys{})
			reader.EnableShared(shared)
			writer.EnableShared(other)
			read, written := make(chan error, 1), make(chan error, 1)
			go func() {
				_, content, err := reader.Get(t.Context(), "alice", "a.json")
				if string(content) != "old" {
					t.Errorf("read content %q, want old", content)
				}
				read <- err
			}()
			<-gate.entered
			var finished atomic.Bool
			go func() {
				var err error
				if remove {
					_, err = writer.Delete(t.Context(), "alice", []string{"a.json"})
				} else {
					_, err = writer.Put(t.Context(), "alice", "a.json", []byte("new"), Origin{})
				}
				written <- err
				finished.Store(true)
			}()
			waiting := advisoryWaiting(t, dsn)
			clustertest.WaitFor(t, "the writer waits for the reader", func() bool { return finished.Load() || waiting() })
			if finished.Load() {
				t.Error("the writer removed the object before its reader finished")
			}
			resume()
			for _, done := range []chan error{read, written} {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func openReplicas(t *testing.T) (*cluster.Store, *cluster.Store, string) {
	t.Helper()
	shared, dsn := clustertest.OpenDSN(t)
	other, err := cluster.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	return shared, other, dsn
}

func advisoryWaiting(t *testing.T, dsn string) func() bool {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return func() bool {
		var waiting bool
		err := conn.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database()))").Scan(&waiting)
		return err == nil && waiting
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
