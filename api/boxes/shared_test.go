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
	a, b, _ := newSharedPair(t, t.Context())
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
	if _, err := a.svc.state.swapHead(t.Context(), p, head{Latest: "stale", version: 1}); err == nil || !strings.Contains(err.Error(), "another api process") {
		t.Errorf("swapHead on an old version = %v, want a refusal", err)
	}
}

func TestAHeadsNotifyFillsTheCacheWithoutDroppingANewerHead(t *testing.T) {
	a, b, db := newSharedPair(t, t.Context())
	p, mark := statePath{user: alice, box: aliceBox}, statePath{user: alice, box: "cb-mark"}
	settle := func(latest string) {
		t.Helper()
		puts := 0
		waitFor(t, "both take a notify of "+latest, func() bool {
			if puts%10 == 0 {
				if err := db.PutHead(t.Context(), alice, mark.box, headBytes(head{Latest: latest, SavedAtMs: int64(puts)})); err != nil {
					t.Fatalf("PutHead: %v", err)
				}
			}
			puts++
			return cachedHead(a, mark).Latest == latest && cachedHead(b, mark).Latest == latest
		})
	}
	settle("listening")
	a.agent.chunks = hexIDs("c1")
	a.save(t)
	settle("after")
	if h := cachedHead(a, p); h.Latest != snapshotID(1) || h.version != cachedHead(b, p).version {
		t.Fatalf("a caches %+v after its own notify, b %+v; want the same head", h, cachedHead(b, p))
	}
	if err := b.svc.state.learnHead(cluster.Head{User: alice, Box: aliceBox, Head: headBytes(head{Latest: "old"}), Version: 0}); err != nil {
		t.Fatalf("learnHead: %v", err)
	}
	if h := cachedHead(b, p); h.Latest != snapshotID(1) {
		t.Errorf("an older head replaced the cached one: %+v", h)
	}
}

func TestAnOperationOfOneProcessRunsAndEndsInTheOther(t *testing.T) {
	a, b, _ := newSharedPair(t, t.Context())
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
	a, b, _ := newSharedPair(t, t.Context())
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
	aCtx, stopA := context.WithCancel(t.Context())
	a, b, _ := newSharedPair(t, aCtx)
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
	_, b, db := newSharedPair(t, t.Context())
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

// newSharedPair returns two services on one machine backend, store and shared database, as two api processes see them; a's process runs until aCtx ends.
func newSharedPair(t *testing.T, aCtx context.Context) (*stateEnv, *stateEnv, *cluster.Store) {
	t.Helper()
	db := clustertest.Open(t)
	a := newStateEnv(t)
	b := &stateEnv{ctx: auth.WithUser(t.Context(), alice), fake: a.fake, agent: a.agent, store: a.store, keys: a.keys}
	b.svc = New(a.fake, testDefaults, a.svc.registry)
	b.svc.EnableState(a.agent, a.store, a.keys)
	a.svc.EnableLedger(aCtx, db)
	a.svc.EnableShared(aCtx, db)
	b.svc.EnableLedger(t.Context(), db)
	b.svc.EnableShared(t.Context(), db)
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

func cachedHead(env *stateEnv, p statePath) head {
	env.svc.state.mu.Lock()
	defer env.svc.state.mu.Unlock()
	return env.svc.state.heads[p]
}
