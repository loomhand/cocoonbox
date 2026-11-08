package boxes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

const (
	alice    = "alice"
	aliceBox = "cb-2bd806c97f0e"
	bob      = "bob"

	refreshEvent = -1
)

var (
	defaultFlavor = machines.Flavor{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40}
	testDefaults  = Defaults{
		Image: "desktop:v2", RebaseImages: []string{"desktop:v3"}, MaxRunning: 1,
	}
)

func TestBoxID(t *testing.T) {
	id := BoxID(alice)
	if !regexp.MustCompile(`^cb-[0-9a-f]{12}$`).MatchString(id) || id != aliceBox {
		t.Fatalf("BoxID(alice) = %q, want %s", id, aliceBox)
	}
	if BoxID(bob) == id {
		t.Error("BoxID must differ per user")
	}
}

func TestLatestStaysInTheImageFamily(t *testing.T) {
	d := Defaults{Image: "cbdesk:v25", RebaseImages: []string{"cbcore:v2", "cbdesk:v24", "cbcore:v1", "host:5000/cbcore:v3"}}
	tests := []struct{ image, want string }{
		{"cbdesk:v22", "cbdesk:v25"},
		{"cbdesk:v25", "cbdesk:v25"},
		{"cbcore:v1", "cbcore:v2"},
		{"cbdroid:v3", "cbdroid:v3"},
		{"", "cbdesk:v25"},
		{"host:5000/cbcore:v1", "host:5000/cbcore:v3"},
		{"host:5000/cbdesk:v1", "host:5000/cbdesk:v1"},
	}
	for _, tt := range tests {
		if got := d.latest(tt.image); got != tt.want {
			t.Errorf("latest(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

func TestAFamilyIsTheNameBeforeTheTagAfterTheLastSlash(t *testing.T) {
	for image, want := range map[string]string{
		"cbcore:v1":           "cbcore",
		"cbcore":              "cbcore",
		"host:5000/cbcore:v1": "host:5000/cbcore",
		"host:5000/cbcore":    "host:5000/cbcore",
		"":                    "",
	} {
		if got := Family(image); got != want {
			t.Errorf("Family(%q) = %q, want %q", image, got, want)
		}
	}
	d := Defaults{Image: "cbcore:v2", RebaseImages: []string{"host:5000/cbcore:v3"}}
	if got, ok := d.offered("host:5000/cbcore"); !ok || got != "host:5000/cbcore:v3" {
		t.Errorf("offered(host:5000/cbcore) = %q, %v, want host:5000/cbcore:v3", got, ok)
	}
	if got, ok := d.offered("host"); ok {
		t.Errorf("offered(host) = %q, want refused", got)
	}
}

func TestTheImageOfAUsersPolicyStartsItsBoxesAndUpgradesThoseOfItsFamily(t *testing.T) {
	fake := newFakeMachines()
	st := newFakeStore()
	defaults := testDefaults
	defaults.Image, defaults.RebaseImages, defaults.MaxRunning = "cbdesk:v25", []string{"cbcore:v2", "cbcore:v1"}, 9
	registry := newFakeRegistry()
	registry.setPolicy(alice, users.Policy{Image: "cbcore:v1"})
	registry.setPolicy("carol", users.Policy{Image: "cbcore:v0"})
	registry.setPolicy("dave", users.Policy{Image: "gone:v1"})
	svc := newService(fake, defaults, registry)
	svc.EnableState(newFakeAgent(st), st, &fakeKeys{})
	for _, tt := range []struct {
		user, want string
	}{
		{alice, "cbcore:v1"},
		{bob, ""},
		{"carol", "cbcore:v2"},
		{"dave", ""},
	} {
		ctx := auth.WithUser(t.Context(), tt.user)
		if _, err := svc.Ensure(ctx, &controlv1.EnsureRequest{}); err != nil || fake.lastSpec().Image != tt.want {
			t.Errorf("Ensure of %s's first box = %v, spec image %q, want %q", tt.user, err, fake.lastSpec().Image, tt.want)
		}
		if _, err := svc.Create(ctx, &controlv1.CreateBoxRequest{Name: "second"}); err != nil || fake.lastSpec().Image != tt.want {
			t.Errorf("Create of %s = %v, spec image %q, want %q", tt.user, err, fake.lastSpec().Image, tt.want)
		}
		if _, err := svc.Create(ctx, &controlv1.CreateBoxRequest{Name: "family", Image: "cbcore"}); err != nil || fake.lastSpec().Image != "cbcore:v2" {
			t.Errorf("Create(cbcore) of %s = %v, spec image %q, want cbcore:v2", tt.user, err, fake.lastSpec().Image)
		}
	}

	if got := defaults.upgrade(svc.ownImage(bob), "cbdesk:v25"); got != "cbdesk:v25" {
		t.Errorf("bob's upgrade = %q, want cbdesk:v25", got)
	}

	for _, tt := range []struct{ image, want string }{
		{"cbcore:v2", "cbcore:v1"},
		{"cbdesk:v24", "cbdesk:v25"},
	} {
		running := newFakeMachines()
		running.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Image: tt.image, Revision: 1})
		svc = newService(running, defaults, registry)
		svc.EnableState(newFakeAgent(st), st, &fakeKeys{})
		ctx := auth.WithUser(t.Context(), alice)
		box, err := svc.get(ctx, aliceBox)
		if err != nil || box.GetLatestImage() != tt.want {
			t.Fatalf("box on %s = %v, %v, want latest %s for its owner", tt.image, box, err, tt.want)
		}
		if _, err := svc.Rebase(ctx, &controlv1.RebaseRequest{BoxId: aliceBox}); err != nil {
			t.Fatalf("Rebase of the box on %s: %v", tt.image, err)
		}
		if spec := running.waitSpec(t, 1); spec.Image != tt.want {
			t.Errorf("upgrade target of the box on %s = %q, want %s", tt.image, spec.Image, tt.want)
		}
	}
}

func TestListNamesTheOfferedImagesOnce(t *testing.T) {
	defaults := testDefaults
	defaults.Image, defaults.RebaseImages = "cbcore:v25", []string{"cbcore:v25", "cbdroid:v3", "cbcore:v24"}
	resp, err := newService(newFakeMachines(), defaults, newFakeRegistry()).List(auth.WithUser(t.Context(), alice), &controlv1.ListBoxesRequest{})
	if want := []string{"cbcore:v25", "cbdroid:v3", "cbcore:v24"}; err != nil || !slices.Equal(resp.GetOfferedImages(), want) {
		t.Errorf("List offered %v, %v, want %v", resp.GetOfferedImages(), err, want)
	}
}

func TestEnsureCreatesBox(t *testing.T) {
	fake := newFakeMachines()
	svc := newService(fake, testDefaults, newFakeRegistry())
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

	fake.push(machines.Machine{ID: aliceBox, State: machines.StateStarting, Detail: "vm=creating"})
	fake.push(machines.Machine{ID: aliceBox, State: machines.StateRunning, Detail: "vm=ready"})
	events := watch(t, svc, ctx, resp.GetOperation().GetId(), "")
	last := events[len(events)-1]
	if last.GetPhase() != controlv1.Phase_PHASE_DONE || last.GetBoxState() != controlv1.BoxState_BOX_STATE_RUNNING || last.GetDetail() != "running" {
		t.Errorf("last event = %v, want DONE/RUNNING", last)
	}
	for i, ev := range events[:len(events)-1] {
		if ev.GetPhase() != controlv1.Phase_PHASE_CREATING || ev.GetDetail() != "starting" {
			t.Errorf("event %d = %s %q, want CREATING starting", i, ev.GetPhase(), ev.GetDetail())
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
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Revision: 1})
	svc := newService(fake, testDefaults, newFakeRegistry())
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
		{name: "machine failed", event: machines.Machine{ID: aliceBox, State: machines.StateFailed, Reason: "push http://192.0.2.10:5000/x: refused", Detail: "vm=failed"}, detail: "machine failed to start"},
		{name: "machine gone", event: machines.Machine{ID: aliceBox, State: machines.StateGone}, detail: "box was deleted"},
		{name: "node at capacity", event: machines.Machine{ID: aliceBox, State: machines.StateFailed, Reason: machines.ErrCapacity.Error()}, detail: machines.ErrCapacity.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeMachines()
			svc := newService(fake, testDefaults, newFakeRegistry())
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
				if strings.Contains(ev.GetDetail(), "192.0.2.10") || strings.Contains(ev.GetDetail(), "vm=") {
					t.Errorf("event leaks a backend message: %q", ev.GetDetail())
				}
			}
		})
	}
}

func TestSupersededOperationFails(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Revision: 1})
	svc := newService(fake, testDefaults, newFakeRegistry())
	ctx := auth.WithUser(t.Context(), alice)
	hibernate, err := svc.Hibernate(ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	fake.setState(aliceBox, machines.StateHibernated)
	wake, err := svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	events := watch(t, svc, ctx, hibernate.GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_FAILED || last.GetDetail() != "superseded by operation "+wake.GetOperation().GetId() {
		t.Errorf("last event = %v", last)
	}
}

func TestEnsureOnAHibernatingBoxAsksAgainAndKeepsTheHibernation(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Revision: 1})
	svc := newService(fake, testDefaults, newFakeRegistry())
	ctx := auth.WithUser(t.Context(), alice)
	hibernate, err := svc.Hibernate(ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	if resp, err := svc.Ensure(ctx, &controlv1.EnsureRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("Ensure on a hibernating box = %v, %v, want Unavailable", resp, err)
	}
	fake.push(machines.Machine{ID: aliceBox, State: machines.StateHibernated})
	events := watch(t, svc, ctx, hibernate.GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_DONE || last.GetBoxState() != controlv1.BoxState_BOX_STATE_HIBERNATED {
		t.Errorf("hibernate after the refused Ensure ended with %v, want DONE/HIBERNATED", last)
	}
}

func TestOwnership(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning})
	svc := newService(fake, testDefaults, newFakeRegistry())
	bobCtx := auth.WithUser(t.Context(), bob)

	if _, err := svc.get(bobCtx, aliceBox); status.Code(err) != codes.NotFound {
		t.Errorf("Get other's box: %v, want NotFound", err)
	}
	if _, err := svc.Hibernate(bobCtx, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.NotFound {
		t.Errorf("Hibernate other's box: %v, want NotFound", err)
	}
	_, _, foreign := svc.Resolve(bobCtx, bob, aliceBox)
	if status.Code(foreign) != codes.NotFound {
		t.Errorf("Resolve other's box: %v, want NotFound", foreign)
	}
	if _, _, unknown := svc.Resolve(bobCtx, bob, "cb-000000000000"); status.Convert(unknown).Proto().String() != status.Convert(foreign).Proto().String() {
		t.Errorf("Resolve of an unknown box = %v, want the same answer as another user's box (%v)", unknown, foreign)
	}
	if list, err := svc.List(bobCtx, &controlv1.ListBoxesRequest{}); err != nil || len(list.GetBoxes()) != 1 || list.GetBoxes()[0].GetId() != BoxID(bob) {
		t.Errorf("List for bob = %v, %v, want only bob's box", list, err)
	}
	if box, err := svc.get(bobCtx, BoxID(bob)); err != nil || box.GetState() != controlv1.BoxState_BOX_STATE_UNSPECIFIED || box.GetName() != users.FirstBoxName {
		t.Errorf("Get own box without a machine = %v, %v, want it absent", box, err)
	}
	if _, err := svc.get(t.Context(), aliceBox); status.Code(err) != codes.Unauthenticated {
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
	if err := svc.WatchOperation(&controlv1.WatchOperationRequest{OperationId: op.GetId()}, &eventStream{ctx: bobCtx}); status.Code(err) != codes.NotFound {
		t.Errorf("WatchOperation of other's operation: %v, want NotFound", err)
	}
}

func TestEnsureErrors(t *testing.T) {
	tests := []struct {
		name    string
		ensure  error
		want    codes.Code
		message string
	}{
		{name: "conflict", ensure: machines.ErrConflict, want: codes.Aborted, message: "ensure box: concurrent change, retry"},
		{name: "backend down", ensure: errors.New("apiserver unreachable"), want: codes.Unavailable, message: "ensure box failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeMachines()
			fake.ensureErr = tt.ensure
			svc := newService(fake, testDefaults, newFakeRegistry())
			_, err := svc.Ensure(auth.WithUser(t.Context(), alice), &controlv1.EnsureRequest{})
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
		{name: "never started or parked", addrErr: machines.ErrNotFound, want: codes.Unavailable, message: "box is not running; call box_ensure to bring it up"},
		{name: "hibernated", addrErr: &machines.NotRunningError{State: machines.StateHibernated}, want: codes.Unavailable, message: "box is hibernated; call box_ensure to bring it up"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeMachines()
			fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning})
			fake.addr, fake.addrErr = "10.203.0.10:7443", tt.addrErr
			ep, _, err := newService(fake, testDefaults, newFakeRegistry()).Resolve(t.Context(), alice, aliceBox)
			if status.Code(err) != tt.want || (err != nil && status.Convert(err).Message() != tt.message) {
				t.Errorf("Resolve: %v, want %s %q", err, tt.want, tt.message)
			}
			if err == nil && ep.Key != "10.203.0.10:7443" {
				t.Errorf("Resolve = %q", ep.Key)
			}
		})
	}
}

func newService(backend machines.Machines, defaults Defaults, registry Registry) *Service {
	s := New(backend, defaults, registry)
	s.EnableAccounts(&fakeAccounts{})
	s.EnableUsage(&fakeMeter{}, 0)
	s.EnableWake(&fakeWakeStore{schedules: map[string]users.WakeSchedule{}}, testWakeLimits)
	s.EnableProvisioning(fakeSigner{})
	s.EnableVault(&fakeVault{digests: map[string]string{}})
	return s
}

// fakeRegistry gives every user the first box BoxID(user), as users.Service does at sign-up.
type fakeRegistry struct {
	mu       sync.Mutex
	boxes    map[string][]users.Box
	max      int
	running  map[string]int
	policies map[string]users.Policy
	changed  func(owner string)
	flavor   machines.Flavor
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{boxes: map[string][]users.Box{}, max: 3, running: map[string]int{}, policies: map[string]users.Policy{}, flavor: defaultFlavor}
}

func (r *fakeRegistry) Boxes(owner string) []users.Box {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.boxes[owner]; !ok {
		r.boxes[owner] = []users.Box{{ID: BoxID(owner), Owner: owner, Name: users.FirstBoxName, Flavor: defaultFlavor}}
	}
	return slices.Clone(r.boxes[owner])
}

func (r *fakeRegistry) HasBox(owner, id string) bool {
	return slices.ContainsFunc(r.Boxes(owner), func(b users.Box) bool { return b.ID == id })
}

func (r *fakeRegistry) AddBox(_ context.Context, b users.Box) (users.Box, error) {
	list := r.Boxes(b.Owner)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !b.Agent && len(slices.DeleteFunc(slices.Clone(list), func(x users.Box) bool { return x.Agent })) >= r.max:
		return users.Box{}, users.ErrQuota
	case slices.ContainsFunc(list, func(x users.Box) bool { return x.Name == b.Name }):
		return users.Box{}, users.ErrExists
	}
	b.CreatedAt, b.Flavor = time.Unix(int64(len(list)+1), 0), r.flavor
	r.boxes[b.Owner] = append(list, b)
	return b, nil
}

func (r *fakeRegistry) RenameBox(_ context.Context, owner, id, name string) (users.Box, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.ContainsFunc(r.boxes[owner], func(x users.Box) bool { return x.Name == name }) {
		return users.Box{}, users.ErrExists
	}
	for i, b := range r.boxes[owner] {
		if b.ID == id {
			r.boxes[owner][i].Name = name
			return r.boxes[owner][i], nil
		}
	}
	return users.Box{}, users.ErrNotFound
}

func (r *fakeRegistry) OnBoxes(fn func(owner string)) {
	r.changed = fn
}

func (r *fakeRegistry) Policy(owner string) users.Policy {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policies[owner]
}

func (r *fakeRegistry) MaxRunning(owner string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[owner]
}

func (r *fakeRegistry) RemoveBox(_ context.Context, owner, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.boxes[owner] = slices.DeleteFunc(r.boxes[owner], func(b users.Box) bool { return b.ID == id })
	return nil
}

func (r *fakeRegistry) setPolicy(owner string, p users.Policy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policies[owner] = p
}

type execCall struct {
	machine    string
	identity   machines.Identity
	gwKey      string
	connectKey string
}

// credCall is one SetCredentials: the values it set and, on a running machine, its flags.
type credCall struct {
	machine string
	secrets map[string]string
	flags   map[string]string
}

// fakeMachines keeps machines in memory; each pushed event reaches exactly one live Watch.
type fakeMachines struct {
	mu          sync.Mutex
	machines    map[string]machines.Machine
	deleted     []string
	clock       int64
	specs       []machines.Spec
	pending     []machines.Machine
	pushed      chan struct{}
	changed     chan struct{}
	lists       int
	revision    int64
	ensureErr   error
	deleteErr   error
	onDeliver   func()
	addr        string
	addrErr     error
	execErr     error
	execs       []execCall
	egress      []string
	creds       []credCall
	keyPushes   []credCall
	credErr     error
	held        map[string]map[string]string
	drops       int
	keepMissing bool
}

func newFakeMachines() *fakeMachines {
	return &fakeMachines{machines: map[string]machines.Machine{}, held: map[string]map[string]string{}, pushed: make(chan struct{}), changed: make(chan struct{})}
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
			ID: spec.ID, Owner: spec.Owner, State: machines.StateStarting,
			Image: cmp.Or(spec.Image, "desktop:v1"), CreatedAt: time.Unix(f.clock, 0), Revision: 1, Meta: maps.Clone(spec.Meta),
		}
		f.machines[spec.ID] = m
		return m, machines.ActionCreated, nil
	case m.State == machines.StateHibernated:
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
	f.lists++
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

func (f *fakeMachines) Changes(string) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (f *fakeMachines) Address(_ context.Context, id string) (machines.Endpoint, error) {
	if f.addrErr != nil {
		return machines.Endpoint{}, f.addrErr
	}
	return machines.TCPEndpoint(cmp.Or(f.addr, id+":7443")), nil
}

func (f *fakeMachines) SetMeta(_ context.Context, id, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return machines.ErrNotFound
	}
	m.Meta = maps.Clone(m.Meta)
	if m.Meta == nil {
		m.Meta = map[string]string{}
	}
	m.Meta[key] = value
	f.machines[id] = m
	f.pending = append(f.pending, machines.Machine{ID: id, Revision: refreshEvent})
	close(f.pushed)
	f.pushed = make(chan struct{})
	return nil
}

func (f *fakeMachines) Deliver(_ context.Context, id string, identity machines.Identity) error {
	if f.onDeliver != nil {
		f.onDeliver()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execErr != nil {
		return f.execErr
	}
	held := f.held[id]
	call := execCall{machine: id, identity: identity, gwKey: strings.TrimPrefix(held[gwKeyEnv], "Bearer "), connectKey: strings.TrimPrefix(held[connectKeyEnv], "Bearer ")}
	f.execs = append(f.execs, call)
	f.filled(id)
	return nil
}

func (f *fakeMachines) SetCredentials(_ context.Context, id string, c machines.Credentials) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return machines.ErrNotFound
	}
	if f.credErr != nil {
		return f.credErr
	}
	if f.held[id] == nil {
		f.held[id] = map[string]string{}
	}
	call := credCall{machine: id, secrets: map[string]string{}}
	for name, sec := range c.Secrets {
		if sec != nil {
			call.secrets[name], f.held[id][name] = sec.Value, sec.Value
		} else {
			delete(f.held[id], name)
		}
	}
	if m.State == machines.StateRunning {
		call.flags = c.Flags
	}
	if c.Flags == nil && c.Prune == "" {
		f.keyPushes = append(f.keyPushes, call)
	} else {
		f.creds = append(f.creds, call)
	}
	f.filled(id)
	return nil
}

func (f *fakeMachines) DropMissing(_ context.Context, id string, _ []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return nil, machines.ErrNotFound
	}
	f.drops++
	m.EnvMissing = false
	f.machines[id] = m
	return []string{"STALE"}, nil
}

func (f *fakeMachines) SetEgress(_ context.Context, id, upstream string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return machines.ErrNotFound
	}
	m.Routed = upstream != ""
	f.machines[id] = m
	f.egress = append(f.egress, id+" "+cmp.Or(upstream, "direct"))
	return nil
}

func (f *fakeMachines) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.machines, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeMachines) filled(id string) {
	if m, ok := f.machines[id]; ok && !f.keepMissing {
		m.EnvMissing = false
		f.machines[id] = m
	}
}

func (f *fakeMachines) execCalls() []execCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.execs)
}

func (f *fakeMachines) credCalls() []credCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.creds)
}

func (f *fakeMachines) keyCalls() []credCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.keyPushes)
}

func (f *fakeMachines) egressCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.egress)
}

func (f *fakeMachines) put(m machines.Machine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.CreatedAt.IsZero() {
		f.clock++
		m.CreatedAt = time.Unix(f.clock, 0)
	}
	f.machines[m.ID] = provisioned(m)
	close(f.changed)
	f.changed = make(chan struct{})
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
	if m.Revision == refreshEvent {
		return cur, true, f.pushed
	}
	m.Owner, m.Image, m.CreatedAt = cmp.Or(m.Owner, cur.Owner), cmp.Or(m.Image, cur.Image), cur.CreatedAt
	m.Incarnation = cmp.Or(m.Incarnation, cur.Incarnation)
	if m.Meta == nil {
		m.Meta = cur.Meta
	}
	m = provisioned(m)
	f.machines[m.ID] = m
	return m, true, f.pushed
}

func (f *fakeMachines) squeeze() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, m := range f.machines {
		m.Squeezed = true
		f.machines[id] = m
	}
}

func (f *fakeMachines) state(id string) machines.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.machines[id].State
}

func (f *fakeMachines) lastSpec() machines.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specs[len(f.specs)-1]
}

func (f *fakeMachines) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

func (f *fakeMachines) watchRevision() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revision
}

func (f *fakeMachines) waitSpec(t *testing.T, n int) machines.Spec {
	t.Helper()
	clustertest.WaitFor(t, fmt.Sprintf("Ensure call %d", n), func() bool { return f.specCount() >= n })
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specs[n-1]
}

func (f *fakeMachines) deletedIDs() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.deleted, ",")
}

func (f *fakeMachines) specCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

func (f *fakeMachines) setState(id string, state machines.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.machines[id]
	m.State = state
	f.machines[id] = provisioned(m)
}

func (f *fakeMachines) setMeta(id, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.machines[id]
	m.Meta = maps.Clone(m.Meta)
	m.Meta[key] = value
	f.machines[id] = m
}

func (f *fakeMachines) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.machines, id)
}

func provisioned(m machines.Machine) machines.Machine {
	if m.State != machines.StateRunning || m.Incarnation != "" {
		return m
	}
	m.Incarnation, m.Meta = "i0", maps.Clone(m.Meta)
	if m.Meta == nil {
		m.Meta = map[string]string{}
	}
	m.Meta[metaWake] = "i0|w0"
	return m
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
