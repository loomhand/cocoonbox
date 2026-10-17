package cluster_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
)

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
	if recent, err := st.Operations(t.Context(), time.Now().Add(-time.Hour)); err != nil || len(recent) != 0 {
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
