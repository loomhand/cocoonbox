package boxes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

func TestTwoProcessesCommitHeadsInTurnAndRefuseAStaleOne(t *testing.T) {
	a, b, _ := newSharedPair(t)
	a.agent.chunks = hexIDs("c1")
	a.save(t)
	p := statePath{user: alice, box: aliceBox}
	waitFor(t, "b sees a's head and no operation", func() bool {
		h, err := b.svc.state.head(t.Context(), p)
		return err == nil && h.Latest == snapshotID(1) && b.svc.ops.activeFor(aliceBox) == nil
	})
	b.agent.chunks = hexIDs("c1", "c2")
	b.save(t)
	waitFor(t, "a sees b's head and no operation", func() bool {
		h, err := a.svc.state.head(t.Context(), p)
		return err == nil && h.Latest == snapshotID(2) && a.svc.ops.activeFor(aliceBox) == nil
	})
	a.save(t)
	if parent := a.agent.lastPlan().GetParent(); parent != snapshotID(2) {
		t.Errorf("a planned on %q, want b's snapshot", parent)
	}
	if _, err := a.svc.state.swapHead(t.Context(), p, head{Latest: "stale"}, 1); err == nil || !strings.Contains(err.Error(), "another api process") {
		t.Errorf("swapHead on an old version = %v, want a refusal", err)
	}
}

func TestAnOperationOfOneProcessRunsAndEndsInTheOther(t *testing.T) {
	a, b, _ := newSharedPair(t)
	gate := make(chan struct{})
	a.agent.onUpload = func(machines.Endpoint) { <-gate }
	a.agent.chunks = hexIDs("c1")
	op, err := a.svc.SaveState(a.ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	waitFor(t, "b sees a's save running", func() bool {
		active := b.svc.ops.activeFor(aliceBox)
		return active != nil && active.id == op.GetId() && active.remote
	})
	if _, err := b.svc.SaveState(b.ctx, &controlv1.BoxRef{BoxId: aliceBox}); err == nil {
		t.Error("b started a save while a's ran")
	}
	close(gate)
	events := watch(t, b.svc, b.ctx, op.GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("b watched a's save end with %v", last)
	}
	waitFor(t, "b forgets a's finished save", func() bool { return b.svc.ops.activeFor(aliceBox) == nil })
}

func TestAnOperationAnotherProcessSupersedesFailsWhereItRuns(t *testing.T) {
	a, b, _ := newSharedPair(t)
	gate := make(chan struct{})
	defer close(gate)
	a.agent.onUpload = func(machines.Endpoint) { <-gate }
	a.agent.chunks = hexIDs("c1")
	op, err := a.svc.SaveState(a.ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	waitFor(t, "b sees a's save running", func() bool { return b.svc.ops.activeFor(aliceBox) != nil })
	next, started := b.svc.ops.start(b.ctx, alice, aliceBox, controlv1.Phase_PHASE_DELETING, 1, time.Minute)
	if !started {
		t.Fatal("b did not start its operation over a's")
	}
	defer next.succeed("done")
	waitFor(t, "a's save fails as superseded", func() bool {
		local := a.svc.ops.local(op.GetId())
		at, done := local.finished()
		return done && !at.IsZero() && strings.Contains(local.last.detail+lastEventDetail(local), "superseded by operation "+next.id)
	})
	if d, err := a.svc.Verify(t.Context()); err != nil || len(d.Operations) != 0 {
		t.Errorf("Verify on a after the supersede = %+v, %v, want the table's events kept", d, err)
	}
}

func TestTheOperationsOfAStoppedProcessEndAndFreeTheBox(t *testing.T) {
	db := clustertest.Open(t)
	aCtx, stopA := context.WithCancel(t.Context())
	a, b := newStateEnv(t), (*stateEnv)(nil)
	b = &stateEnv{ctx: auth.WithUser(t.Context(), alice), fake: a.fake, agent: a.agent, store: a.store, keys: a.keys}
	b.svc = New(a.fake, testDefaults, a.svc.registry)
	b.svc.EnableState(a.agent, a.store, a.keys)
	a.svc.EnableLedger(aCtx, db)
	a.svc.EnableShared(aCtx, db)
	b.svc.EnableLedger(t.Context(), db)
	b.svc.EnableShared(t.Context(), db)
	gate := make(chan struct{})
	defer close(gate)
	a.agent.onUpload = func(machines.Endpoint) { <-gate }
	a.agent.chunks = hexIDs("c1")
	op, err := a.svc.SaveState(a.ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	waitFor(t, "b sees a's save running", func() bool { return b.svc.ops.activeFor(aliceBox) != nil })
	stopA()
	waitFor(t, "b sees the box free once a stopped", func() bool { return b.svc.ops.activeFor(aliceBox) == nil })
	events := watch(t, b.svc, b.ctx, op.GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_FAILED || !strings.Contains(last.GetDetail(), "stopped") {
		t.Errorf("a's save ended with %v, want a failure saying its process stopped", last)
	}
}

func TestAnOperationPastItsLeaseCountsAsGone(t *testing.T) {
	_, b, db := newSharedPair(t)
	if err := db.PutOperation(t.Context(), cluster.Operation{
		ID: "op-dead", User: alice, Box: aliceBox, Phase: int32(controlv1.Phase_PHASE_SAVING), Events: []byte(`[]`),
		ExpiresAt: time.Now().Add(200 * time.Millisecond), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("PutOperation: %v", err)
	}
	waitFor(t, "b sees the operation", func() bool { return b.svc.ops.activeFor(aliceBox) != nil })
	waitFor(t, "b takes the operation as gone past its lease", func() bool { return b.svc.ops.activeFor(aliceBox) == nil })
	events := watch(t, b.svc, b.ctx, "op-dead", "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_FAILED {
		t.Errorf("watching an operation past its lease ended with %v", last)
	}
}

func TestAClientOfOneProcessKeepsTheBoxUpForTheOthersIdleSweep(t *testing.T) {
	db := clustertest.Open(t)
	a, clock := newUserIdleEnv(t)
	b := &stateEnv{ctx: auth.WithUser(t.Context(), alice), fake: a.fake, agent: a.agent, store: a.store, keys: a.keys}
	b.svc = New(a.fake, testDefaults, a.svc.registry)
	b.svc.EnableState(a.agent, a.store, a.keys)
	b.svc.EnableIdle(0, userIdleAfter)
	b.svc.idle.now = a.svc.idle.now
	for _, env := range []*stateEnv{a, b} {
		env.svc.EnableLedger(t.Context(), db)
		env.svc.EnableShared(t.Context(), db)
	}
	a.svc.hibernateIdle(t.Context())
	done := b.svc.Reached(aliceBox)
	defer done()
	*clock = clock.Add(userIdleAfter)
	b.svc.reaches.flush(t.Context(), b.svc.openBoxes())
	a.svc.hibernateIdle(t.Context())
	if op := newestOperation(a.svc, aliceBox); op != nil || a.fake.state(aliceBox) != machines.StateRunning {
		t.Errorf("a's sweep started an operation (%t) on a box a client of b holds, state %v", op != nil, a.fake.state(aliceBox))
	}
}

func TestTheFirstReachAfterAQuietSpellReachesTheTableAtOnce(t *testing.T) {
	_, b, db := newSharedPair(t)
	b.svc.Reached(aliceBox)()
	waitFor(t, "the reach is in the table before the next flush", func() bool {
		seen, err := db.Activity(t.Context(), time.Now().Add(-time.Minute))
		return err == nil && !seen[aliceBox].IsZero()
	})
}

// newSharedPair returns two services on one machine backend, store and shared database, as two api processes see them.
func newSharedPair(t *testing.T) (*stateEnv, *stateEnv, *cluster.Store) {
	t.Helper()
	db := clustertest.Open(t)
	a := newStateEnv(t)
	b := &stateEnv{ctx: auth.WithUser(t.Context(), alice), fake: a.fake, agent: a.agent, store: a.store, keys: a.keys}
	b.svc = New(a.fake, testDefaults, a.svc.registry)
	b.svc.EnableState(a.agent, a.store, a.keys)
	for _, env := range []*stateEnv{a, b} {
		env.svc.EnableLedger(t.Context(), db)
		env.svc.EnableShared(t.Context(), db)
	}
	return a, b, db
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 5 s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lastEventDetail(op *operation) string {
	op.mu.Lock()
	defer op.mu.Unlock()
	if len(op.events) == 0 {
		return ""
	}
	return op.events[len(op.events)-1].GetDetail()
}
