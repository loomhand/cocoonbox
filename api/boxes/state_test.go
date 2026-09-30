package boxes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/dialer"
	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/keyring"
	"github.com/CMGS/cocoonbox/api/machines"
	"github.com/CMGS/cocoonbox/api/store"
)

func TestSaveStateIsIncremental(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{"c1", "c2", "c3"}
	events := env.save(t)
	last := events[len(events)-1]
	if last.GetPhase() != controlv1.Phase_PHASE_DONE || !strings.Contains(last.GetDetail(), "snapshot s1") {
		t.Fatalf("last event = %v", last)
	}
	if !hasProgress(events) {
		t.Errorf("no event carried transfer bytes: %v", events)
	}
	plan := env.agent.lastPlan()
	if plan.GetConsistency() != agentv1.Consistency_CONSISTENCY_QUIESCED || plan.GetParent() != "" || len(plan.GetKey().GetKey()) != keyring.KeySize {
		t.Errorf("first plan = %v", plan)
	}
	if got := env.agent.lastUpload(); len(got.GetPuts()) != 3 || got.GetManifest().GetUrl() != "fake://put/"+aliceBox+"/manifests/s1" {
		t.Errorf("first upload = %v", got)
	}

	env.agent.chunks = []string{"c1", "c2", "c4", "c4"}
	env.save(t)
	if plan := env.agent.lastPlan(); plan.GetParent() != "s1" {
		t.Errorf("second plan parent = %q, want s1", plan.GetParent())
	}
	puts := env.agent.lastUpload().GetPuts()
	if len(puts) != 1 || puts[0].GetChunkId() != "c4" {
		t.Errorf("second upload puts = %v, want only c4", puts)
	}
	box, err := env.svc.Get(env.ctx, &controlv1.GetBoxRequest{BoxId: aliceBox})
	if err != nil || box.GetLastSavedAtMs() == 0 || box.GetLatestImage() != "desktop-v2" {
		t.Errorf("box = %v, %v", box, err)
	}
	if head := string(env.store.objects[aliceBox+"/head.json"]); !strings.Contains(head, `"latest":"s2"`) {
		t.Errorf("head = %s", head)
	}
}

func TestSaveStateRefusals(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated})
	svc := New(fake, testDefaults)
	ctx := auth.WithUser(t.Context(), alice)
	if _, err := svc.SaveState(ctx, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SaveState without a store: %v, want FailedPrecondition", err)
	}
	svc.EnableState(&fakeAgent{}, newFakeStore(), &fakeKeys{})
	if _, err := svc.SaveState(ctx, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SaveState on a hibernated box: %v, want FailedPrecondition", err)
	}
	if _, err := svc.SaveState(auth.WithUser(t.Context(), "bob"), &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("SaveState on another user's box: %v, want PermissionDenied", err)
	}
	if _, err := svc.Rebase(ctx, &controlv1.RebaseRequest{BoxId: aliceBox}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Rebase on a hibernated box: %v, want FailedPrecondition", err)
	}
}

func TestSchedulerSavesStaleRunningBoxes(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{"c1"}
	env.svc.saveStale(env.ctx, time.Hour)
	op := env.svc.ops.activeFor(aliceBox)
	if op == nil {
		t.Fatal("scheduler started no save")
	}
	events := watch(t, env.svc, env.ctx, op.id, "")
	if events[len(events)-1].GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("scheduled save = %v", events)
	}
	if c := env.agent.lastPlan().GetConsistency(); c != agentv1.Consistency_CONSISTENCY_LIVE {
		t.Errorf("scheduled save consistency = %s, want LIVE", c)
	}
	env.svc.saveStale(env.ctx, time.Hour)
	if active := env.svc.ops.activeFor(aliceBox); active != nil {
		t.Errorf("scheduler saved again within the interval: %s", active.id)
	}
	if n := env.agent.plans(); n != 1 {
		t.Errorf("agent planned %d times, want 1", n)
	}
}

func TestRebase(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{"c1", "c2"}
	op, err := env.svc.Rebase(env.ctx, &controlv1.RebaseRequest{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	newID := env.fake.waitSpec(t, 1).ID
	if !strings.HasPrefix(newID, aliceBox+"-r") {
		t.Fatalf("rebase machine id = %q", newID)
	}
	if addr, err := env.svc.Resolve(env.ctx, alice, aliceBox); err != nil || addr != aliceBox+":7443" {
		t.Errorf("during rebase Resolve = %q, %v, want the old machine", addr, err)
	}
	env.fake.push(machines.Machine{ID: newID, State: machines.StateRunning})
	events := watch(t, env.svc, env.ctx, op.GetId(), "")
	last := events[len(events)-1]
	if last.GetPhase() != controlv1.Phase_PHASE_DONE || !strings.Contains(last.GetDetail(), "now on "+newID+" (desktop-v2)") {
		t.Fatalf("last event = %v", last)
	}
	phases := map[controlv1.Phase]bool{}
	for _, ev := range events {
		phases[ev.GetPhase()] = true
	}
	for _, p := range []controlv1.Phase{controlv1.Phase_PHASE_SAVING, controlv1.Phase_PHASE_CREATING, controlv1.Phase_PHASE_RESTORING} {
		if !phases[p] {
			t.Errorf("no %s event in %v", p, events)
		}
	}
	if spec := env.fake.lastSpec(); spec.Image != "desktop-v2" || spec.Owner != alice || spec.Flavor != defaultFlavor {
		t.Errorf("rebase spec = %+v", spec)
	}
	if c := env.agent.lastPlan().GetConsistency(); c != agentv1.Consistency_CONSISTENCY_QUIESCED {
		t.Errorf("rebase save consistency = %s, want QUIESCED", c)
	}
	restore := env.agent.lastRestore()
	if restore.addr != newID+":7443" || len(restore.req.GetGets()) != 2 || restore.req.GetManifest().GetChunkId() != "s1" {
		t.Errorf("restore = %s %v", restore.addr, restore.req)
	}
	if env.fake.deletedIDs() != aliceBox {
		t.Errorf("deleted = %q, want the old machine", env.fake.deletedIDs())
	}
	if addr, err := env.svc.Resolve(env.ctx, alice, aliceBox); err != nil || addr != newID+":7443" {
		t.Errorf("after rebase Resolve = %q, %v, want the new machine", addr, err)
	}
}

func TestRebaseKeepsOldMachineWhenRestoreFails(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{"c1"}
	env.agent.restoreErr = errors.New("disk full")
	op, err := env.svc.Rebase(env.ctx, &controlv1.RebaseRequest{BoxId: aliceBox, Image: "desktop-v3"})
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	newID := env.fake.waitSpec(t, 1).ID
	env.fake.push(machines.Machine{ID: newID, State: machines.StateRunning})
	events := watch(t, env.svc, env.ctx, op.GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_FAILED || !strings.Contains(last.GetDetail(), "disk full") {
		t.Errorf("last event = %v", last)
	}
	if env.fake.deletedIDs() != newID {
		t.Errorf("deleted = %q, want only the new machine", env.fake.deletedIDs())
	}
	if addr, err := env.svc.Resolve(env.ctx, alice, aliceBox); err != nil || addr != aliceBox+":7443" {
		t.Errorf("Resolve = %q, %v, want the old machine", addr, err)
	}
}

type stateEnv struct {
	ctx   context.Context
	svc   *Service
	fake  *fakeMachines
	agent *fakeAgent
	store *fakeStore
}

func newStateEnv(t *testing.T) *stateEnv {
	t.Helper()
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor, Image: "desktop-v1", Revision: 1})
	st := newFakeStore()
	agent := &fakeAgent{store: st}
	svc := New(fake, testDefaults)
	svc.EnableState(agent, st, &fakeKeys{})
	return &stateEnv{ctx: auth.WithUser(t.Context(), alice), svc: svc, fake: fake, agent: agent, store: st}
}

func (e *stateEnv) save(t *testing.T) []*controlv1.OperationEvent {
	t.Helper()
	op, err := e.svc.SaveState(e.ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	events := watch(t, e.svc, e.ctx, op.GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("save ended with %v", last)
	}
	return events
}

func (f *fakeMachines) waitSpec(t *testing.T, n int) machines.Spec {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.specs) >= n {
			spec := f.specs[n-1]
			f.mu.Unlock()
			return spec
		}
		f.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no Ensure call %d", n)
	return machines.Spec{}
}

func (f *fakeMachines) deletedIDs() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.deleted, ",")
}

func hasProgress(events []*controlv1.OperationEvent) bool {
	for _, ev := range events {
		if ev.GetTotalBytes() > 0 {
			return true
		}
	}
	return false
}

type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{objects: map[string][]byte{}} }

func (s *fakeStore) PresignPut(_ context.Context, key string) (store.Presigned, error) {
	return store.Presigned{URL: "fake://put/" + key}, nil
}

func (s *fakeStore) PresignGet(_ context.Context, key string) (store.Presigned, error) {
	return store.Presigned{URL: "fake://get/" + key}, nil
}

func (s *fakeStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return data, nil
}

func (s *fakeStore) Put(_ context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
	return nil
}

type fakeKeys struct{}

func (fakeKeys) Key(_ context.Context, owner string) (keyring.Key, error) {
	return keyring.Key{ID: "test:" + owner, Secret: make([]byte, keyring.KeySize)}, nil
}

type restoreCall struct {
	addr string
	req  *agentv1.RestoreRequest
}

// fakeAgent plans snapshots s1, s2, ... over chunks and moves objects in the fake store as presigned URLs name them.
type fakeAgent struct {
	store      *fakeStore
	chunks     []string
	restoreErr error

	mu        sync.Mutex
	planReqs  []*agentv1.PlanRequest
	uploads   []*agentv1.UploadRequest
	restores  []restoreCall
	snapshots int
}

func (a *fakeAgent) Plan(_ context.Context, _ string, req *agentv1.PlanRequest) (*agentv1.PlanResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.planReqs = append(a.planReqs, req)
	a.snapshots++
	resp := &agentv1.PlanResponse{SnapshotId: fmt.Sprintf("s%d", a.snapshots), Files: 7, LogicalBytes: 3 << 20, TakenAtMs: time.Now().UnixMilli()}
	for _, id := range a.chunks {
		resp.Chunks = append(resp.Chunks, &agentv1.ChunkRef{Id: id, Size: 1 << 20})
	}
	return resp, nil
}

func (a *fakeAgent) Upload(_ context.Context, _ string, req *agentv1.UploadRequest, progress dialer.ProgressFunc) (*agentv1.TransferProgress, error) {
	a.mu.Lock()
	a.uploads = append(a.uploads, req)
	a.mu.Unlock()
	total := uint64(len(req.GetPuts())) << 20
	for i, put := range append(req.GetPuts(), req.GetManifest()) {
		if err := a.store.Put(context.Background(), strings.TrimPrefix(put.GetUrl(), "fake://put/"), []byte(put.GetChunkId())); err != nil {
			return nil, err
		}
		progress(&agentv1.TransferProgress{DoneBytes: min(uint64(i+1)<<20, total), TotalBytes: total})
	}
	final := &agentv1.TransferProgress{DoneBytes: total, TotalBytes: total, Finished: true, ManifestId: req.GetSnapshotId()}
	progress(final)
	return final, nil
}

func (a *fakeAgent) Restore(_ context.Context, addr string, req *agentv1.RestoreRequest, progress dialer.ProgressFunc) (*agentv1.TransferProgress, error) {
	a.mu.Lock()
	a.restores = append(a.restores, restoreCall{addr: addr, req: req})
	a.mu.Unlock()
	if a.restoreErr != nil {
		return nil, a.restoreErr
	}
	for _, get := range append(req.GetGets(), req.GetManifest()) {
		if _, err := a.store.Get(context.Background(), strings.TrimPrefix(get.GetUrl(), "fake://get/")); err != nil {
			return nil, fmt.Errorf("get %s: %w", get.GetUrl(), err)
		}
	}
	final := &agentv1.TransferProgress{Finished: true, ManifestId: req.GetManifest().GetChunkId()}
	progress(final)
	return final, nil
}

func (a *fakeAgent) lastPlan() *agentv1.PlanRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planReqs[len(a.planReqs)-1]
}

func (a *fakeAgent) plans() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.planReqs)
}

func (a *fakeAgent) lastUpload() *agentv1.UploadRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.uploads[len(a.uploads)-1]
}

func (a *fakeAgent) lastRestore() restoreCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.restores[len(a.restores)-1]
}
