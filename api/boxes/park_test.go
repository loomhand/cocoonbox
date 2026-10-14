package boxes

import (
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

const parkAfter = 24 * time.Hour

func TestParkSavesAndRemovesABoxHibernatedPastTheThreshold(t *testing.T) {
	e := newParkEnv(t)
	start := e.clock
	if got := e.meta(metaSlept); got != strconv.FormatInt(start.UnixMilli(), 10) {
		t.Errorf("hibernation stamp = %q, want %d", got, start.UnixMilli())
	}
	if op := e.park(t, start.Add(parkAfter-time.Minute)); op != nil || e.fake.specCount() != 0 {
		t.Fatal("a box hibernated under the threshold was parked")
	}

	op := e.park(t, start.Add(parkAfter))
	if op == nil {
		t.Fatal("a box hibernated past the threshold was not parked")
	}
	last := lastEvent(op)
	if last.GetPhase() != controlv1.Phase_PHASE_DONE || !strings.Contains(last.GetDetail(), "parked: snapshot "+snapshotID(2)) {
		t.Fatalf("last event = %v", last)
	}
	if spec := e.fake.lastSpec(); spec.ID != aliceBox {
		t.Errorf("woke %q, want the hibernated machine", spec.ID)
	}
	if plan := e.agent.lastPlan(); plan.GetConsistency() != agentv1.Consistency_CONSISTENCY_QUIESCED || plan.GetParent() != snapshotID(1) {
		t.Errorf("park plan = %v, want a quiesced save on the last snapshot", plan)
	}
	if e.fake.deletedIDs() != aliceBox {
		t.Errorf("deleted = %q, want the parked machine", e.fake.deletedIDs())
	}
	if h, err := e.svc.state.head(e.ctx, statePath{user: alice, box: aliceBox}); err != nil || !h.Parked || h.Latest != snapshotID(2) {
		t.Errorf("head = %+v, %v, want the park snapshot marked parked", h, err)
	}
	box, err := e.svc.Get(e.ctx, &controlv1.GetBoxRequest{BoxId: aliceBox})
	if err != nil || box.GetState() != controlv1.BoxState_BOX_STATE_UNSPECIFIED || box.GetLastSavedAtMs() == 0 {
		t.Errorf("parked box = %v, %v", box, err)
	}

	if _, err := e.svc.Ensure(e.ctx, &controlv1.EnsureRequest{BoxId: aliceBox}); err != nil {
		t.Fatalf("Ensure after the park: %v", err)
	}
	spec := e.fake.waitSpec(t, 2)
	if !strings.HasPrefix(spec.ID, aliceBox+"-r") {
		t.Fatalf("restore machine = %q, want a new %s-r machine", spec.ID, aliceBox)
	}
	e.fake.push(machines.Machine{ID: spec.ID, State: machines.StateRunning})
	e.idle(t)
	if restore := e.agent.lastRestore(); restore.addr != spec.ID+":7443" || restore.reqs[0].GetManifest().GetChunkId() != snapshotID(2) {
		t.Errorf("restore = %s %v, want the park snapshot", restore.addr, restore.reqs)
	}
}

func TestParkLeavesABoxAlone(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *parkEnv)
	}{
		{"running", func(e *parkEnv) { e.fake.setState(aliceBox, machines.StateRunning) }},
		{"hibernated again lately", func(e *parkEnv) {
			e.fake.setMeta(aliceBox, metaSlept, strconv.FormatInt(e.clock.Add(time.Hour).UnixMilli(), 10))
		}},
		{"wake due within the threshold", func(e *parkEnv) {
			e.wakes().put(users.WakeSchedule{Box: aliceBox, Owner: alice, NextAt: e.clock.Add(parkAfter + time.Hour)})
		}},
		{"held awake by its wake", func(e *parkEnv) {
			e.wakes().put(users.WakeSchedule{Box: aliceBox, Owner: alice, NextAt: e.clock.Add(3 * parkAfter), SleepAt: e.clock.Add(parkAfter)})
		}},
		{"another operation", func(e *parkEnv) {
			e.svc.ops.start(e.ctx, alice, aliceBox, controlv1.Phase_PHASE_DELETING, 1, time.Minute)
		}},
		{"box deleted", func(e *parkEnv) { _ = e.svc.registry.RemoveBox(e.ctx, alice, aliceBox) }},
		{"owner outside park_owners", func(e *parkEnv) { e.svc.park.owners = []string{bob} }},
		{"a second machine", func(e *parkEnv) {
			e.fake.put(machines.Machine{ID: aliceBox + "-rx", Owner: alice, State: machines.StateStarting})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newParkEnv(t)
			tt.setup(e)
			if op := e.park(t, e.clock.Add(parkAfter)); op != nil || e.fake.specCount() != 0 || e.fake.deletedIDs() != "" {
				t.Errorf("parked: %t, %d Ensure calls, deleted %q", op != nil, e.fake.specCount(), e.fake.deletedIDs())
			}
		})
	}
}

func TestParkOwnersListingTheOwnerParksTheBox(t *testing.T) {
	e := newParkEnv(t)
	e.svc.park.owners = []string{bob, alice}
	if op := e.park(t, e.clock.Add(parkAfter)); op == nil || e.fake.deletedIDs() != aliceBox {
		t.Errorf("parked: %t, deleted %q, want alice's box parked", op != nil, e.fake.deletedIDs())
	}
}

func TestAClientDuringAParkKeepsTheMachine(t *testing.T) {
	e := newParkEnv(t)
	gate := make(chan struct{})
	e.agent.onUpload = func(machines.Endpoint) { <-gate }
	e.clock = e.clock.Add(parkAfter)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.svc.parkIdle(t.Context())
	}()
	e.waitFor(t, func() bool { return e.fake.state(aliceBox) == machines.StateStarting })
	parkOp := e.svc.ops.activeFor(aliceBox)
	e.fake.push(machines.Machine{ID: aliceBox, State: machines.StateRunning})
	e.waitFor(t, func() bool { return e.agent.plans() == 2 })

	if _, _, err := e.svc.Resolve(e.ctx, alice, aliceBox); status.Code(err) != codes.Unavailable {
		t.Errorf("Resolve while parking = %v, want Unavailable", err)
	}
	if box, _ := e.svc.Get(e.ctx, &controlv1.GetBoxRequest{BoxId: aliceBox}); box.GetState() != controlv1.BoxState_BOX_STATE_STARTING {
		t.Errorf("box while parking = %v, want STARTING", box)
	}
	if _, err := e.svc.Hibernate(e.ctx, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Hibernate while parking = %v, want FailedPrecondition", err)
	}
	resp, err := e.svc.Ensure(e.ctx, &controlv1.EnsureRequest{BoxId: aliceBox})
	if err != nil || resp.GetOperation().GetId() == "" || resp.GetOperation().GetId() == parkOp.id {
		t.Fatalf("Ensure while parking = %v, %v, want an operation of its own", resp, err)
	}
	close(gate)
	<-done

	if last := lastEvent(parkOp); last.GetPhase() != controlv1.Phase_PHASE_FAILED || !strings.Contains(last.GetDetail(), "superseded") {
		t.Errorf("park ended with %v, want superseded", last)
	}
	if e.fake.deletedIDs() != "" || e.fake.state(aliceBox) != machines.StateRunning {
		t.Errorf("deleted %q, state %v; want the machine kept running", e.fake.deletedIDs(), e.fake.state(aliceBox))
	}
	e.fake.push(machines.Machine{ID: aliceBox, Revision: refreshEvent})
	if last := watch(t, e.svc, e.ctx, resp.GetOperation().GetId(), ""); last[len(last)-1].GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("Ensure ended with %v", last[len(last)-1])
	}
	if _, _, err := e.svc.Resolve(e.ctx, alice, aliceBox); err != nil {
		t.Errorf("Resolve after the Ensure = %v", err)
	}
}

func TestABoxBeingParkedHoldsNoRunningSlot(t *testing.T) {
	e := newParkEnv(t)
	other, err := e.svc.registry.AddBox(e.ctx, users.Box{ID: "cb-000000000002", Owner: alice, Name: "second"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	e.fake.setState(aliceBox, machines.StateRunning)
	if _, err := e.svc.Ensure(e.ctx, &controlv1.EnsureRequest{BoxId: other.ID}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Ensure beside a running box = %v, want FailedPrecondition", err)
	}
	e.svc.ops.begin(e.ctx, &operation{user: alice, boxID: aliceBox, phase: controlv1.Phase_PHASE_SAVING, park: true}, time.Minute)
	if _, err := e.svc.Ensure(e.ctx, &controlv1.EnsureRequest{BoxId: other.ID}); err != nil {
		t.Errorf("Ensure beside a box being parked = %v", err)
	}
	if _, err := e.svc.Ensure(e.ctx, &controlv1.EnsureRequest{BoxId: aliceBox}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Ensure of the box being parked beside a running box = %v, want FailedPrecondition", err)
	}
}

func TestAFailedParkHibernatesTheBoxAgain(t *testing.T) {
	e := newParkEnv(t)
	e.svc.state.retryDelay = time.Millisecond
	e.store.failNext(statePath{user: alice, box: aliceBox}.head(), 10)
	at := e.clock.Add(parkAfter)
	op := e.park(t, at)
	if op == nil {
		t.Fatal("no park")
	}
	if last := lastEvent(op); last.GetPhase() != controlv1.Phase_PHASE_FAILED || !strings.Contains(last.GetDetail(), "hibernated again") {
		t.Errorf("park ended with %v", last)
	}
	if e.fake.deletedIDs() != "" || e.fake.state(aliceBox) != machines.StateHibernating {
		t.Errorf("deleted %q, state %v; want the machine hibernating again", e.fake.deletedIDs(), e.fake.state(aliceBox))
	}
	e.fake.setState(aliceBox, machines.StateHibernated)
	if op := e.park(t, at.Add(parkAfter-time.Minute)); op != nil {
		t.Error("parked again within the threshold of the failed park")
	}
}

// parkEnv holds alice's box hibernated at clock, after one save.
type parkEnv struct {
	*stateEnv
	clock time.Time
}

func newParkEnv(t *testing.T) *parkEnv {
	t.Helper()
	e := &parkEnv{stateEnv: newStateEnv(t)}
	e.agent.chunks = []string{hexID("c1")}
	e.save(t)
	e.clock = time.Now()
	e.svc.EnableParking(parkAfter, nil)
	e.svc.park.now = func() time.Time { return e.clock }
	if _, err := e.svc.Hibernate(e.ctx, &controlv1.BoxRef{BoxId: aliceBox}); err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	e.fake.push(machines.Machine{ID: aliceBox, State: machines.StateHibernated})
	e.idle(t)
	return e
}

// park runs one sweep at now, letting a machine it wakes come up, and returns the park operation, nil when it parked nothing.
func (e *parkEnv) park(t *testing.T, now time.Time) *operation {
	t.Helper()
	e.clock = now
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.svc.parkIdle(t.Context())
	}()
	var op *operation
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-done:
			return op
		case <-deadline:
			t.Fatal("the sweep did not end")
		case <-time.After(5 * time.Millisecond):
		}
		if op == nil && e.fake.state(aliceBox) == machines.StateStarting {
			op = e.svc.ops.activeFor(aliceBox)
			e.fake.push(machines.Machine{ID: aliceBox, State: machines.StateRunning})
		}
	}
}

func (e *parkEnv) wakes() *fakeWakeStore {
	store := &fakeWakeStore{schedules: map[string]users.WakeSchedule{}}
	e.svc.EnableWake(store, testWakeLimits)
	return store
}

func (e *parkEnv) meta(key string) string {
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	return e.fake.machines[aliceBox].Meta[key]
}

func (e *parkEnv) idle(t *testing.T) {
	t.Helper()
	e.waitFor(t, func() bool { return e.svc.ops.activeFor(aliceBox) == nil })
}

func (e *parkEnv) waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fakeMachines) setState(id string, state machines.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.machines[id]
	m.State = state
	f.machines[id] = m
}

func (f *fakeMachines) setMeta(id, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.machines[id]
	m.Meta = maps.Clone(m.Meta)
	m.Meta[key] = value
	f.machines[id] = m
}

func lastEvent(op *operation) *controlv1.OperationEvent {
	events, _, _ := op.since(0)
	return events[len(events)-1]
}
