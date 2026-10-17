package cluster_test

import (
	"context"
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
)

func TestPendingMigrationsListsWhatOpenAppliesAndWritesNothing(t *testing.T) {
	dsn := clustertest.EmptyDSN(t)
	pending, err := cluster.PendingMigrations(t.Context(), dsn)
	if err != nil || len(pending) == 0 {
		t.Fatalf("PendingMigrations on an empty database = %v, %v", pending, err)
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	var tables int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("tables after PendingMigrations = %d, %v; want none", tables, err)
	}
	st, err := cluster.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Close()
	if pending, err := cluster.PendingMigrations(t.Context(), dsn); err != nil || len(pending) != 0 {
		t.Errorf("PendingMigrations after Open = %v, %v; want none", pending, err)
	}
}

func TestPutMachineRoundTripsEveryField(t *testing.T) {
	st := clustertest.Open(t)
	want := cluster.Machine{
		ID: "cb-1", Owner: "u-1", Image: "desk:v1", CreatedAt: time.Now(), Claim: []byte(`{"id":"sbx-1", "hibernated":false}`),
		OwnerAddr: "10.0.0.1:17780", Op: "wake", OpErr: "", OpUntil: time.Now().Add(time.Minute), Revision: 7,
		Meta: map[string]string{"slept": "1"}, Wakes: 3, Routed: true, Ready: true, ReadyRestarts: 2,
	}
	if err := st.PutMachine(t.Context(), want); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	got, err := st.Machine(t.Context(), "cb-1")
	if err != nil || !got.Equal(want) || got.Version == 0 {
		t.Fatalf("Machine = %+v, %v, want %+v with a version", got, err, want)
	}
	first := got.Version
	want.Claim, want.Op, want.OpUntil, want.Env = nil, "", time.Time{}, map[string]string{"GW_KEY": "f1"}
	if err := st.PutMachine(t.Context(), want); err != nil {
		t.Fatalf("PutMachine again: %v", err)
	}
	if got, err = st.Machine(t.Context(), "cb-1"); err != nil || !got.Equal(want) || got.Version <= first {
		t.Fatalf("Machine after the second put = %+v, %v, want a version past %d", got, err, first)
	}
	if err := st.DeleteMachine(t.Context(), "cb-1"); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}
	if _, err = st.Machine(t.Context(), "cb-1"); !errors.Is(err, cluster.ErrNotFound) {
		t.Errorf("Machine after delete = %v, want ErrNotFound", err)
	}
	if err := st.DeleteMachine(t.Context(), "cb-1"); err != nil {
		t.Errorf("DeleteMachine of a missing machine: %v", err)
	}
}

func TestEqualComparesEveryFieldAsTheRowStoresIt(t *testing.T) {
	at := time.Now()
	m := cluster.Machine{ID: "cb-1", CreatedAt: at, Meta: map[string]string{}, Claim: []byte{}, Version: 3}
	stored := cluster.Machine{ID: "cb-1", CreatedAt: at.Truncate(time.Microsecond).In(time.FixedZone("x", 3600)), Version: 9}
	if !m.Equal(stored) {
		t.Errorf("%+v and %+v differ only as the row stores them", m, stored)
	}
	for _, change := range []func(*cluster.Machine){
		func(m *cluster.Machine) { m.OwnerAddr = "10.0.0.2:17780" },
		func(m *cluster.Machine) { m.ReadyRestarts = 1 },
		func(m *cluster.Machine) { m.OpUntil = at.Add(time.Second) },
	} {
		changed := m
		change(&changed)
		if m.Equal(changed) {
			t.Errorf("%+v equals %+v", m, changed)
		}
	}
}

func TestUpdateMachineRerunsTheEditOnAConcurrentWrite(t *testing.T) {
	st := clustertest.Open(t)
	if err := st.PutMachine(t.Context(), cluster.Machine{ID: "cb-1", Owner: "u-1", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	put, err := st.Machine(t.Context(), "cb-1")
	if err != nil {
		t.Fatalf("Machine: %v", err)
	}
	runs := 0
	got, err := st.UpdateMachine(t.Context(), "cb-1", func(m *cluster.Machine, found bool) (bool, error) {
		runs++
		if runs == 1 {
			if _, err := st.UpdateMachine(t.Context(), "cb-1", func(m *cluster.Machine, _ bool) (bool, error) {
				m.Revision++
				return true, nil
			}); err != nil {
				t.Errorf("inner UpdateMachine: %v", err)
			}
		}
		if !found || m.Op != "" {
			return false, nil
		}
		m.Op, m.Revision = "wake", m.Revision+1
		return true, nil
	})
	if err != nil || runs != 2 || got.Op != "wake" || got.Revision != 2 || got.Version <= put.Version {
		t.Errorf("UpdateMachine = %+v, %v after %d runs, want the edit on top of the concurrent one", got, err, runs)
	}
}

func TestUpdateMachineInsertsOnceForConcurrentCreators(t *testing.T) {
	st := clustertest.Open(t)
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Go(func() {
			m, err := st.UpdateMachine(t.Context(), "cb-1", func(m *cluster.Machine, found bool) (bool, error) {
				if found {
					return false, nil
				}
				m.Owner, m.CreatedAt, m.Op = "u-1", time.Now(), "claim"
				return true, nil
			})
			if err != nil {
				t.Errorf("UpdateMachine: %v", err)
			}
			results[i] = m.Op
		})
	}
	wg.Wait()
	all, err := st.Machines(t.Context())
	if err != nil || len(all) != 1 {
		t.Fatalf("Machines = %+v, %v, want one row", all, err)
	}
	if slices.Contains(results, "") {
		t.Errorf("results = %q, want every caller to see the claim", results)
	}
}

func TestDeleteMachineIfRemovesOnlyWhileTheConditionHolds(t *testing.T) {
	st := clustertest.Open(t)
	if err := st.PutMachine(t.Context(), cluster.Machine{ID: "cb-1", Owner: "u-1", CreatedAt: time.Now(), Op: "wake"}); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	idle := func(m cluster.Machine) bool { return m.Op == "" }
	if removed, err := st.DeleteMachineIf(t.Context(), "cb-1", idle); err != nil || removed {
		t.Fatalf("DeleteMachineIf during an op = %t, %v, want kept", removed, err)
	}
	if _, err := st.UpdateMachine(t.Context(), "cb-1", func(m *cluster.Machine, _ bool) (bool, error) {
		m.Op = ""
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateMachine: %v", err)
	}
	if removed, err := st.DeleteMachineIf(t.Context(), "cb-1", idle); err != nil || !removed {
		t.Fatalf("DeleteMachineIf when idle = %t, %v, want removed", removed, err)
	}
	if removed, err := st.DeleteMachineIf(t.Context(), "cb-1", idle); !errors.Is(err, cluster.ErrNotFound) || removed {
		t.Errorf("DeleteMachineIf of a missing machine = %t, %v, want ErrNotFound", removed, err)
	}
}

func TestSwapHeadWritesOnlyOnTheVersionRead(t *testing.T) {
	st := clustertest.Open(t)
	v1, err := st.SwapHead(t.Context(), "u-1", "cb-1", []byte(`{"latest":"a"}`), 0)
	if err != nil || v1 != 1 {
		t.Fatalf("SwapHead absent = %d, %v", v1, err)
	}
	if _, err := st.SwapHead(t.Context(), "u-1", "cb-1", []byte(`{"latest":"b"}`), 0); !errors.Is(err, cluster.ErrStale) {
		t.Fatalf("SwapHead absent over a head = %v, want ErrStale", err)
	}
	v2, err := st.SwapHead(t.Context(), "u-1", "cb-1", []byte(`{"latest":"b"}`), v1)
	if err != nil || v2 != 2 {
		t.Fatalf("SwapHead current = %d, %v", v2, err)
	}
	if _, err := st.SwapHead(t.Context(), "u-1", "cb-1", []byte(`{"latest":"c"}`), v1); !errors.Is(err, cluster.ErrStale) {
		t.Errorf("SwapHead stale = %v, want ErrStale", err)
	}
	if err := st.PutHead(t.Context(), "u-1", "cb-1", []byte(`{"latest":"d"}`)); err != nil {
		t.Errorf("PutHead: %v", err)
	}
	if err := st.PutHead(t.Context(), "u-1", "cb-1", []byte(`{"latest":"d"}`)); err != nil {
		t.Errorf("PutHead of the stored head: %v", err)
	}
	h, err := st.Head(t.Context(), "u-1", "cb-1")
	if err != nil || string(h.Head) != `{"latest":"d"}` || h.Version != 3 {
		t.Errorf("Head = %+v, %v", h, err)
	}
	if _, err := st.Head(t.Context(), "u-1", "cb-2"); !errors.Is(err, cluster.ErrNotFound) {
		t.Errorf("Head of a box without one = %v, want ErrNotFound", err)
	}
}

func TestOperationsKeepTheirEventsUntilPurged(t *testing.T) {
	st := clustertest.Open(t)
	op := cluster.Operation{ID: "op-1", User: "u-1", Box: "cb-1", Phase: 3, Gen: 4, Events: []byte(`[{"phase":3}]`), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}
	if err := st.PutOperation(t.Context(), op); err != nil {
		t.Fatalf("PutOperation: %v", err)
	}
	op.Events, op.FinishedAt = []byte(`[{"phase":3},{"phase":5}]`), time.Now().Add(-2*time.Hour)
	if err := st.PutOperation(t.Context(), op); err != nil {
		t.Fatalf("PutOperation finished: %v", err)
	}
	got, err := st.Operation(t.Context(), "op-1")
	if err != nil || string(got.Events) != string(op.Events) || got.FinishedAt.IsZero() || got.Gen != 4 {
		t.Fatalf("Operation = %+v, %v", got, err)
	}
	if recent, err := st.Operations(t.Context(), "", time.Now().Add(-time.Hour)); err != nil || len(recent) != 0 {
		t.Errorf("Operations since an hour ago = %v, %v, want none", recent, err)
	}
	abandoned := cluster.Operation{ID: "op-2", User: "u-1", Box: "cb-1", Events: []byte(`[]`), ExpiresAt: time.Now().Add(-2 * time.Hour), CreatedAt: time.Now().Add(-3 * time.Hour)}
	if err := st.PutOperation(t.Context(), abandoned); err != nil {
		t.Fatalf("PutOperation abandoned: %v", err)
	}
	if err := st.PurgeOperations(t.Context(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("PurgeOperations: %v", err)
	}
	if _, err := st.Operation(t.Context(), "op-2"); !errors.Is(err, cluster.ErrNotFound) {
		t.Errorf("an unfinished operation past its expiry after the purge = %v, want ErrNotFound", err)
	}
	if _, err := st.Operation(t.Context(), "op-1"); !errors.Is(err, cluster.ErrNotFound) {
		t.Errorf("Operation after the purge = %v, want ErrNotFound", err)
	}
}

func TestListenDeliversTheChangeOfEveryWrite(t *testing.T) {
	st := clustertest.Open(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready, got := make(chan struct{}), make(chan cluster.Change, 4)
	go st.Listen(ctx, cluster.ChannelMachines, func() { close(ready) }, func(payload string) {
		c, err := cluster.ParseChange(payload)
		if err != nil {
			t.Errorf("ParseChange(%q): %v", payload, err)
		}
		got <- c
	})
	<-ready
	if err := st.PutMachine(t.Context(), cluster.Machine{ID: "cb-1", Owner: "u-1", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	if err := st.DeleteMachine(t.Context(), "cb-1"); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}
	for _, removed := range []bool{false, true} {
		select {
		case c := <-got:
			if c.ID != "cb-1" || (c.Version == 0) != removed {
				t.Errorf("notified %+v, want cb-1 removed %t", c, removed)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no notify within 5 s")
		}
	}
}

func TestOneProcessLeadsAndAnotherTakesOverWhenItsConnectionDies(t *testing.T) {
	st, dsn := clustertest.OpenDSN(t)
	other, err := cluster.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(other.Close)
	var mu sync.Mutex
	leading := map[string]bool{}
	run := func(name string, s *cluster.Store) {
		s.Lead(t.Context(), func(ctx context.Context) {
			mu.Lock()
			if len(leading) > 0 {
				t.Errorf("%s leads while %v does", name, leading)
			}
			leading[name] = true
			mu.Unlock()
			<-ctx.Done()
			mu.Lock()
			delete(leading, name)
			mu.Unlock()
		})
	}
	go run("a", st)
	go run("b", other)
	first := waitLeader(t, &mu, leading, "")
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory' AND granted
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database()) AND classid = $1 AND objid = $2`, 0x6362, 0x6c656164); err != nil {
		t.Fatalf("end the leader's connection: %v", err)
	}
	if next := waitLeader(t, &mu, leading, first); next == first {
		t.Errorf("leader stayed %s after its connection died", first)
	}
}

func TestALeaderWhoseLinkGoesSilentStopsAndAnotherTakesOver(t *testing.T) {
	st, dsn := clustertest.OpenDSN(t)
	link := newFreezer(t, dsn)
	silent, err := cluster.Open(t.Context(), link.dsn)
	if err != nil {
		t.Fatalf("Open through the link: %v", err)
	}
	t.Cleanup(silent.Close)
	var mu sync.Mutex
	leading := map[string]bool{}
	stopped := make(chan time.Time, 1)
	lead := func(name string) cluster.Work {
		return func(ctx context.Context) {
			mu.Lock()
			if len(leading) > 0 {
				t.Errorf("%s leads while %v does", name, leading)
			}
			leading[name] = true
			mu.Unlock()
			<-ctx.Done()
			mu.Lock()
			delete(leading, name)
			mu.Unlock()
			if name == "silent" {
				stopped <- time.Now()
			}
		}
	}
	go silent.Lead(t.Context(), lead("silent"))
	if first := waitLeader(t, &mu, leading, ""); first != "silent" {
		t.Fatalf("leader = %s, want the one behind the link", first)
	}
	go st.Lead(t.Context(), lead("direct"))
	link.freeze()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the leader behind a silent link kept leading")
	}
	if next := waitLeader(t, &mu, leading, "silent"); next != "direct" {
		t.Errorf("leader after the silent link = %s, want direct", next)
	}
}

func TestTheLeaderLeadsWhileStartLocksHoldEveryLockConnection(t *testing.T) {
	st := clustertest.Open(t)
	for _, user := range []string{"u-1", "u-2", "u-3", "u-4"} {
		unlock, err := st.Lock(t.Context(), user)
		if err != nil {
			t.Fatalf("Lock %s: %v", user, err)
		}
		defer unlock()
	}
	leading := make(chan struct{})
	go st.Lead(t.Context(), func(ctx context.Context) {
		close(leading)
		<-ctx.Done()
	})
	select {
	case <-leading:
	case <-time.After(15 * time.Second):
		t.Fatal("the leader waited behind the start locks")
	}
}

func TestAPinHoldsItsSnapshotUntilDeletedOrExpired(t *testing.T) {
	st := clustertest.Open(t)
	if err := st.PutPin(t.Context(), "p-1", "u-1", "cb-1", "s-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("PutPin: %v", err)
	}
	if err := st.PutPin(t.Context(), "p-2", "u-1", "cb-1", "s-2", time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("PutPin expired: %v", err)
	}
	if pins, err := st.Pins(t.Context(), "u-1", "cb-1"); err != nil || !slices.Equal(pins, []string{"s-1"}) {
		t.Fatalf("Pins = %v, %v; want the live pin only", pins, err)
	}
	if err := st.DeletePin(t.Context(), "p-1"); err != nil {
		t.Fatalf("DeletePin: %v", err)
	}
	if pins, err := st.Pins(t.Context(), "u-1", "cb-1"); err != nil || len(pins) != 0 {
		t.Errorf("Pins after the delete = %v, %v", pins, err)
	}
}

func TestAUserLockHeldByOneProcessHoldsOffAnotherUntilReleased(t *testing.T) {
	a, dsn := clustertest.OpenDSN(t)
	b, err := cluster.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(b.Close)
	unlock, err := a.Lock(t.Context(), "u-1")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	other, err := b.Lock(t.Context(), "u-2")
	if err != nil {
		t.Fatalf("Lock of another user: %v", err)
	}
	other()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := b.Lock(ctx, "u-1"); err == nil {
		t.Fatal("b took the lock a holds")
	}
	got := make(chan error, 1)
	go func() {
		release, err := b.Lock(t.Context(), "u-1")
		if err == nil {
			release()
		}
		got <- err
	}()
	unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("Lock after the release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("b did not take the lock a released")
	}
}

func TestFlushKeysWaitsForItsKeysAndReportsTheirFailedWrites(t *testing.T) {
	m := cluster.NewMirror()
	go m.Run(t.Context())
	m.Put("op:ok", func(context.Context) error { return nil })
	m.Put("op:bad", func(context.Context) error { return errors.New("database gone") })
	if err := m.FlushKeys(t.Context(), []string{"op:ok"}); err != nil {
		t.Errorf("FlushKeys of a written key = %v", err)
	}
	if err := m.FlushKeys(t.Context(), []string{"op:bad"}); err == nil || !strings.Contains(err.Error(), "database gone") {
		t.Errorf("FlushKeys of a key whose write failed = %v, want its error", err)
	}
	m.Put("op:bad", func(context.Context) error { return nil })
	if err := m.FlushKeys(t.Context(), []string{"op:bad"}); err != nil {
		t.Errorf("FlushKeys after the key's write succeeded = %v", err)
	}
}

func TestMirrorAppliesTheNewestWritePerKeyInOrder(t *testing.T) {
	m := cluster.NewMirror()
	var mu sync.Mutex
	var applied []string
	write := func(name string) cluster.Write {
		return func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			applied = append(applied, name)
			return nil
		}
	}
	m.Put("a", write("a1"))
	m.Put("b", write("b1"))
	m.Put("a", write("a2"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go m.Run(ctx)
	if err := m.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(applied, []string{"a2", "b1"}) {
		t.Errorf("applied %q, want a2 then b1", applied)
	}
}

func waitLeader(t *testing.T, mu *sync.Mutex, leading map[string]bool, not string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for name := range leading {
			if name != not {
				mu.Unlock()
				return name
			}
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no leader other than %q within 20 s", not)
	return ""
}

// freezer forwards connections to the database of a dsn until freeze, after which it keeps them open and passes nothing.
type freezer struct {
	dsn    string
	frozen atomic.Bool
}

func newFreezer(t *testing.T, dsn string) *freezer {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	target := u.Host
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	u.Host = lis.Addr().String()
	f := &freezer{dsn: u.String()}
	go func() {
		for {
			client, err := lis.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			go f.pipe(client, server)
			go f.pipe(server, client)
		}
	}()
	return f
}

func (f *freezer) freeze() {
	f.frozen.Store(true)
}

func (f *freezer) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if f.frozen.Load() {
			continue
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}
