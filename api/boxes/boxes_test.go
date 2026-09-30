package boxes

import (
	"cmp"
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	alice    = "alice"
	aliceBox = "cb-2bd806c97f0e"
)

var (
	defaultFlavor = machines.Flavor{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40}
	testDefaults  = Defaults{
		Flavor: defaultFlavor, Flavors: []machines.Flavor{{VCPUs: 2, MemoryMiB: 4096, DiskGiB: 40}},
		Image: "desktop-v2", RebaseImages: []string{"desktop-v3"},
	}
)

func TestBoxID(t *testing.T) {
	id := BoxID(alice)
	if !regexp.MustCompile(`^cb-[0-9a-f]{12}$`).MatchString(id) || id != aliceBox {
		t.Fatalf("BoxID(alice) = %q, want %s", id, aliceBox)
	}
	if BoxID("bob") == id {
		t.Error("BoxID must differ per user")
	}
}

func TestEnsureCreatesBox(t *testing.T) {
	fake := newFakeMachines()
	svc := New(fake, testDefaults)
	ctx := auth.WithUser(t.Context(), alice)

	resp, err := svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if resp.GetBox().GetId() != aliceBox || resp.GetBox().GetState() != controlv1.BoxState_BOX_STATE_STARTING {
		t.Errorf("box = %v", resp.GetBox())
	}
	if spec := fake.lastSpec(); spec.ID != aliceBox || spec.Owner != alice || spec.Flavor != defaultFlavor {
		t.Errorf("backend spec = %+v", spec)
	}
	again, err := svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil || again.GetOperation().GetId() != resp.GetOperation().GetId() {
		t.Errorf("second Ensure = %v, %v, want operation %s", again.GetOperation(), err, resp.GetOperation().GetId())
	}

	fake.push(machines.Machine{ID: aliceBox, State: machines.StateStarting, Detail: "vm=creating", Waiting: true})
	fake.push(machines.Machine{ID: aliceBox, State: machines.StateRunning, Detail: "vm=ready"})
	events := watch(t, svc, ctx, resp.GetOperation().GetId(), "")
	last := events[len(events)-1]
	if last.GetPhase() != controlv1.Phase_PHASE_DONE || last.GetBoxState() != controlv1.BoxState_BOX_STATE_RUNNING || last.GetDetail() != "running" {
		t.Errorf("last event = %v, want DONE/RUNNING", last)
	}
	for i, ev := range events[:len(events)-1] {
		if ev.GetPhase() != controlv1.Phase_PHASE_CREATING || ev.GetDetail() != "waiting for capacity" {
			t.Errorf("event %d = %s %q, want CREATING waiting for capacity", i, ev.GetPhase(), ev.GetDetail())
		}
	}
	if got := fake.watchRevision(); got != 1 {
		t.Errorf("watched from revision %d, want the created revision 1", got)
	}

	running, err := svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil || running.GetOperation() != nil || running.GetBox().GetState() != controlv1.BoxState_BOX_STATE_RUNNING {
		t.Errorf("Ensure on a running box = %v, %v", running, err)
	}
}

func TestHibernateAndWake(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor, Revision: 1})
	svc := New(fake, testDefaults)
	ctx := auth.WithUser(t.Context(), alice)

	op, err := svc.Hibernate(ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	fake.push(machines.Machine{ID: aliceBox, State: machines.StateHibernating, Detail: "saving"})
	fake.push(machines.Machine{ID: aliceBox, State: machines.StateHibernated, Detail: "saved"})
	events := watch(t, svc, ctx, op.GetId(), "")
	if len(events) != 2 {
		t.Fatalf("events = %v, want 2", events)
	}
	if events[0].GetPhase() != controlv1.Phase_PHASE_SNAPSHOTTING || events[1].GetPhase() != controlv1.Phase_PHASE_DONE ||
		events[1].GetBoxState() != controlv1.BoxState_BOX_STATE_HIBERNATED {
		t.Errorf("events = %v", events)
	}
	resumed := watch(t, svc, ctx, op.GetId(), events[0].GetOffsetKey())
	if len(resumed) != 1 || resumed[0].GetOffsetKey() != events[1].GetOffsetKey() {
		t.Errorf("resume after %s = %v", events[0].GetOffsetKey(), resumed)
	}

	wake, err := svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure wake: %v", err)
	}
	fake.push(machines.Machine{ID: aliceBox, State: machines.StateStarting, Detail: "vm=creating"})
	fake.push(machines.Machine{ID: aliceBox, State: machines.StateRunning})
	events = watch(t, svc, ctx, wake.GetOperation().GetId(), "")
	if events[0].GetPhase() != controlv1.Phase_PHASE_WAKING || events[len(events)-1].GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Errorf("wake events = %v", events)
	}
}

func TestOperationFails(t *testing.T) {
	tests := []struct {
		name   string
		event  machines.Machine
		detail string
	}{
		{name: "machine failed", event: machines.Machine{ID: aliceBox, State: machines.StateFailed, Reason: "push http://10.90.0.4:5000/x: refused", Detail: "vm=failed"}, detail: "machine failed to start"},
		{name: "machine gone", event: machines.Machine{ID: aliceBox, State: machines.StateGone}, detail: "box was deleted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeMachines()
			svc := New(fake, testDefaults)
			ctx := auth.WithUser(t.Context(), alice)
			resp, err := svc.Ensure(ctx, &controlv1.EnsureRequest{})
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			fake.push(tt.event)
			events := watch(t, svc, ctx, resp.GetOperation().GetId(), "")
			if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_FAILED || last.GetDetail() != tt.detail {
				t.Errorf("last event = %v, want FAILED %q", last, tt.detail)
			}
			for _, ev := range events {
				if strings.Contains(ev.GetDetail(), "10.90.0.4") || strings.Contains(ev.GetDetail(), "vm=") {
					t.Errorf("event leaks a backend message: %q", ev.GetDetail())
				}
			}
			if box, err := svc.Get(ctx, &controlv1.GetBoxRequest{BoxId: aliceBox}); err == nil && strings.Contains(box.GetFailureReason(), "10.90.0.4") {
				t.Errorf("failure_reason leaks a backend message: %q", box.GetFailureReason())
			}
		})
	}
}

func TestSupersededOperationFails(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor, Revision: 1})
	svc := New(fake, testDefaults)
	ctx := auth.WithUser(t.Context(), alice)
	hibernate, err := svc.Hibernate(ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	wake, err := svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	events := watch(t, svc, ctx, hibernate.GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_FAILED || last.GetDetail() != "superseded by operation "+wake.GetOperation().GetId() {
		t.Errorf("last event = %v", last)
	}
}

func TestOwnership(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor})
	svc := New(fake, testDefaults)
	bob := auth.WithUser(t.Context(), "bob")

	if _, err := svc.Get(bob, &controlv1.GetBoxRequest{BoxId: aliceBox}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Get other's box: %v, want PermissionDenied", err)
	}
	if _, err := svc.Hibernate(bob, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Hibernate other's box: %v, want PermissionDenied", err)
	}
	if _, err := svc.Resolve(bob, "bob", aliceBox); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Resolve other's box: %v, want PermissionDenied", err)
	}
	if list, err := svc.List(bob, &controlv1.ListBoxesRequest{}); err != nil || len(list.GetBoxes()) != 0 {
		t.Errorf("List for bob = %v, %v, want empty", list, err)
	}
	if _, err := svc.Get(bob, &controlv1.GetBoxRequest{BoxId: BoxID("bob")}); status.Code(err) != codes.NotFound {
		t.Errorf("Get own absent box: %v, want NotFound", err)
	}
	if _, err := svc.Get(t.Context(), &controlv1.GetBoxRequest{BoxId: aliceBox}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Get without user: %v, want Unauthenticated", err)
	}
	aliceCtx := auth.WithUser(t.Context(), alice)
	if list, err := svc.List(aliceCtx, &controlv1.ListBoxesRequest{}); err != nil || len(list.GetBoxes()) != 1 {
		t.Errorf("List for alice = %v, %v", list, err)
	}
	op, err := svc.Hibernate(aliceCtx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("Hibernate alice: %v", err)
	}
	if err := svc.WatchOperation(&controlv1.WatchOperationRequest{OperationId: op.GetId()}, &eventStream{ctx: bob}); status.Code(err) != codes.NotFound {
		t.Errorf("WatchOperation of other's operation: %v, want NotFound", err)
	}
}

func TestEnsureErrors(t *testing.T) {
	tests := []struct {
		name    string
		stored  *machines.Machine
		flavor  *controlv1.Flavor
		ensure  error
		want    codes.Code
		message string
	}{
		{name: "other flavor", stored: &machines.Machine{ID: aliceBox, Owner: alice, Flavor: defaultFlavor}, flavor: &controlv1.Flavor{Vcpus: 2, MemoryMib: 4096}, want: codes.FailedPrecondition, message: "box exists with flavor vcpus:4 memory_mib:8192 disk_gib:40"},
		{name: "flavor not offered", flavor: &controlv1.Flavor{Vcpus: 64, MemoryMib: 1 << 20, DiskGib: 10000}, want: codes.InvalidArgument, message: "flavor vcpus:64 memory_mib:1048576 disk_gib:10000 is not offered"},
		{name: "offered flavor", flavor: &controlv1.Flavor{Vcpus: 2, MemoryMib: 4096, DiskGib: 40}, want: codes.OK},
		{name: "default flavor spelled out", stored: &machines.Machine{ID: aliceBox, Owner: alice, Flavor: defaultFlavor, State: machines.StateRunning}, flavor: &controlv1.Flavor{Vcpus: 4}, want: codes.OK},
		{name: "deleting", ensure: machines.ErrDeleting, want: codes.FailedPrecondition, message: "box is being deleted"},
		{name: "conflict", ensure: machines.ErrConflict, want: codes.Aborted, message: "ensure box: concurrent change, retry"},
		{name: "backend down", ensure: errors.New("apiserver unreachable"), want: codes.Unavailable, message: "ensure box failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeMachines()
			if tt.stored != nil {
				fake.put(*tt.stored)
			}
			fake.ensureErr = tt.ensure
			svc := New(fake, testDefaults)
			_, err := svc.Ensure(auth.WithUser(t.Context(), alice), &controlv1.EnsureRequest{Flavor: tt.flavor})
			if status.Code(err) != tt.want || (err != nil && status.Convert(err).Message() != tt.message) {
				t.Errorf("Ensure: %v, want %s %q", err, tt.want, tt.message)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name    string
		addrErr error
		want    codes.Code
		message string
	}{
		{name: "running", want: codes.OK},
		{name: "machine vanished", addrErr: machines.ErrNotFound, want: codes.NotFound, message: "box not found"},
		{name: "hibernated", addrErr: &machines.NotRunningError{State: machines.StateHibernated}, want: codes.Unavailable, message: "box is BOX_STATE_HIBERNATED"},
		{name: "no address", addrErr: machines.ErrNoAddress, want: codes.Unavailable, message: "box has no address yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeMachines()
			fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning})
			fake.addr, fake.addrErr = "10.203.0.10:7443", tt.addrErr
			addr, err := New(fake, testDefaults).Resolve(t.Context(), alice, aliceBox)
			if status.Code(err) != tt.want || (err != nil && status.Convert(err).Message() != tt.message) {
				t.Errorf("Resolve: %v, want %s %q", err, tt.want, tt.message)
			}
			if err == nil && addr != "10.203.0.10:7443" {
				t.Errorf("Resolve = %q", addr)
			}
		})
	}
}

// fakeMachines keeps machines in memory; Ensure mirrors the cocoonset backend and each pushed event reaches exactly one live Watch.
type fakeMachines struct {
	mu        sync.Mutex
	machines  map[string]machines.Machine
	deleted   []string
	clock     int64
	specs     []machines.Spec
	pending   []machines.Machine
	pushed    chan struct{}
	revision  int64
	ensureErr error
	addr      string
	addrErr   error
}

func newFakeMachines() *fakeMachines {
	return &fakeMachines{machines: map[string]machines.Machine{}, pushed: make(chan struct{})}
}

func (f *fakeMachines) Ensure(_ context.Context, spec machines.Spec) (machines.Machine, machines.Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	if f.ensureErr != nil {
		return machines.Machine{}, machines.ActionNone, f.ensureErr
	}
	m, ok := f.machines[spec.ID]
	switch {
	case !ok:
		f.clock++
		m = machines.Machine{
			ID: spec.ID, Owner: spec.Owner, State: machines.StateStarting, Flavor: spec.Flavor,
			Image: cmp.Or(spec.Image, "desktop-v1"), CreatedAt: time.Unix(f.clock, 0), Revision: 1,
		}
		f.machines[spec.ID] = m
		return m, machines.ActionCreated, nil
	case m.State == machines.StateHibernated || m.State == machines.StateHibernating:
		m.State, m.Revision = machines.StateStarting, m.Revision+1
		f.machines[spec.ID] = m
		return m, machines.ActionWoken, nil
	default:
		return m, machines.ActionNone, nil
	}
}

func (f *fakeMachines) Get(_ context.Context, id string) (machines.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return machines.Machine{}, machines.ErrNotFound
	}
	return m, nil
}

func (f *fakeMachines) List(_ context.Context, owner string) ([]machines.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []machines.Machine
	for _, m := range f.machines {
		if owner == "" || m.Owner == owner {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeMachines) Hibernate(_ context.Context, id string) (machines.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return machines.Machine{}, machines.ErrNotFound
	}
	m.State, m.Revision = machines.StateHibernating, m.Revision+1
	f.machines[id] = m
	return m, nil
}

func (f *fakeMachines) Watch(ctx context.Context, _ string, revision int64) <-chan machines.Machine {
	f.mu.Lock()
	f.revision = revision
	f.mu.Unlock()
	out := make(chan machines.Machine)
	go func() {
		defer close(out)
		for {
			m, ok, pushed := f.take(ctx)
			if !ok {
				select {
				case <-pushed:
					continue
				case <-ctx.Done():
					return
				}
			}
			select {
			case out <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (f *fakeMachines) Address(_ context.Context, id string) (string, error) {
	if f.addr == "" && f.addrErr == nil {
		return id + ":7443", nil
	}
	return f.addr, f.addrErr
}

func (f *fakeMachines) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.machines, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeMachines) put(m machines.Machine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.CreatedAt.IsZero() {
		f.clock++
		m.CreatedAt = time.Unix(f.clock, 0)
	}
	f.machines[m.ID] = m
}

func (f *fakeMachines) push(m machines.Machine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, m)
	close(f.pushed)
	f.pushed = make(chan struct{})
}

func (f *fakeMachines) take(ctx context.Context) (machines.Machine, bool, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil || len(f.pending) == 0 {
		return machines.Machine{}, false, f.pushed
	}
	m := f.pending[0]
	f.pending = f.pending[1:]
	cur := f.machines[m.ID]
	m.Owner, m.Flavor, m.Image, m.CreatedAt = cmp.Or(m.Owner, cur.Owner), cmp.Or(m.Flavor, cur.Flavor), cmp.Or(m.Image, cur.Image), cur.CreatedAt
	f.machines[m.ID] = m
	return m, true, f.pushed
}

func (f *fakeMachines) lastSpec() machines.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specs[len(f.specs)-1]
}

func (f *fakeMachines) watchRevision() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revision
}

func watch(t *testing.T, svc *Service, ctx context.Context, opID, from string) []*controlv1.OperationEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stream := &eventStream{ctx: ctx}
	if err := svc.WatchOperation(&controlv1.WatchOperationRequest{OperationId: opID, FromOffsetKey: from}, stream); err != nil {
		t.Fatalf("WatchOperation: %v (events so far %v)", err, stream.events)
	}
	if len(stream.events) == 0 {
		t.Fatal("WatchOperation returned no events")
	}
	return stream.events
}

type eventStream struct {
	grpc.ServerStream
	ctx    context.Context
	events []*controlv1.OperationEvent
}

func (s *eventStream) Context() context.Context { return s.ctx }

func (s *eventStream) Send(ev *controlv1.OperationEvent) error {
	s.events = append(s.events, ev)
	return nil
}
