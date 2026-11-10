package boxes

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/dialer"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/store"
	"github.com/loomhand/cocoonbox/api/store/s3"
	"github.com/loomhand/cocoonbox/api/users"
)

const credentialLifetime = 10 * time.Minute

func TestSaveStateIsIncremental(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1"), hexID("c2"), hexID("c3")}
	events := env.save(t)
	last := events[len(events)-1]
	if last.GetPhase() != controlv1.Phase_PHASE_DONE || !strings.Contains(last.GetDetail(), "snapshot "+snapshotID(1)) {
		t.Fatalf("last event = %v", last)
	}
	if !slices.ContainsFunc(events, transfer) {
		t.Errorf("no event carried transfer bytes: %v", events)
	}
	plan := env.agent.lastPlan()
	if plan.GetConsistency() != agentv1.Consistency_CONSISTENCY_QUIESCED || plan.GetParent() != "" || len(plan.GetKey().GetKey()) != keyring.KeySize {
		t.Errorf("first plan = %v", plan)
	}
	if got := env.agent.lastUpload(); len(got.GetPuts()) != 3 || got.GetManifest().GetUrl() != "fake://put/"+alice+"/boxes/"+aliceBox+"/manifests/"+snapshotID(1) {
		t.Errorf("first upload = %v", got)
	}

	env.agent.chunks = []string{hexID("c1"), hexID("c2"), hexID("c4"), hexID("c4")}
	env.save(t)
	if plan := env.agent.lastPlan(); plan.GetParent() != snapshotID(1) {
		t.Errorf("second plan parent = %q, want %s", plan.GetParent(), snapshotID(1))
	}
	puts := env.agent.lastUpload().GetPuts()
	if len(puts) != 1 || puts[0].GetChunkId() != hexID("c4") {
		t.Errorf("second upload puts = %v, want only c4", puts)
	}
	box, err := env.svc.get(env.ctx, aliceBox)
	if err != nil || box.GetLastSavedAtMs() == 0 || box.GetLatestImage() != "desktop:v2" {
		t.Errorf("box = %v, %v", box, err)
	}
	if head := string(env.store.objects[alice+"/boxes/"+aliceBox+"/head.json"]); !strings.Contains(head, `"latest":"`+snapshotID(2)+`"`) {
		t.Errorf("head = %s", head)
	}
}

func TestSaveStateRefusals(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated})
	svc := newService(fake, testDefaults, newFakeRegistry())
	ctx := auth.WithUser(t.Context(), alice)
	if _, err := svc.SaveState(ctx, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SaveState without a store: %v, want FailedPrecondition", err)
	}
	svc.EnableState(newFakeAgent(newFakeStore()), newFakeStore(), &fakeKeys{})
	if _, err := svc.SaveState(ctx, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SaveState on a hibernated box: %v, want FailedPrecondition", err)
	}
	if _, err := svc.SaveState(auth.WithUser(t.Context(), bob), &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.NotFound {
		t.Errorf("SaveState on another user's box: %v, want NotFound", err)
	}
	if _, err := svc.Rebase(ctx, &controlv1.RebaseRequest{BoxId: aliceBox}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Rebase on a hibernated box: %v, want FailedPrecondition", err)
	}
}

func TestSchedulerSavesStaleRunningBoxes(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1")}
	gate := make(chan struct{})
	env.agent.onUpload = func(machines.Endpoint) { <-gate }
	env.svc.saveStale(env.ctx, time.Hour)
	op := env.svc.ops.activeFor(aliceBox)
	close(gate)
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

func TestSchedulerRunsAtMostScheduledSavesAndSkipsABoxThatStoppedWhileWaiting(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1")}
	for i := range 2*scheduledSaves - 1 {
		env.fake.put(machines.Machine{ID: fmt.Sprintf("cb-%012d", i), Owner: alice, State: machines.StateRunning, Revision: 1})
	}
	var mu sync.Mutex
	var inflight, peak int
	uploaded := map[string]bool{}
	gate := make(chan struct{})
	env.agent.onUpload = func(ep machines.Endpoint) {
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		uploaded[ep.Key] = true
		mu.Unlock()
		<-gate
		mu.Lock()
		inflight--
		mu.Unlock()
	}
	done := make(chan struct{})
	go func() {
		env.svc.saveStale(env.ctx, time.Hour)
		close(done)
	}()
	clustertest.WaitFor(t, "every slot holds a save", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return inflight >= scheduledSaves
	})
	ms, _ := env.fake.List(env.ctx, "")
	mu.Lock()
	for _, m := range ms {
		if !uploaded[m.ID+":7443"] {
			m.State = machines.StateHibernated
			env.fake.put(m)
		}
	}
	mu.Unlock()
	for range scheduledSaves {
		gate <- struct{}{}
	}
	<-done
	mu.Lock()
	got := peak
	mu.Unlock()
	if got != scheduledSaves {
		t.Errorf("peak scheduled saves = %d, want %d", got, scheduledSaves)
	}
	if n := env.agent.plans(); n != scheduledSaves {
		t.Errorf("agent planned %d times, want %d: a box hibernated while waiting for a slot was saved", n, scheduledSaves)
	}
}

func TestSchedulerRetriesAFailedSaveAfterAnIntervalNotEveryTick(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1")}
	env.fake.addrErr = errors.New("no route")
	env.svc.saveStale(env.ctx, time.Hour)
	saves := savesOf(env.svc, aliceBox)
	if len(saves) != 1 {
		t.Fatalf("scheduler started %d saves, want 1", len(saves))
	}
	if events := watch(t, env.svc, env.ctx, saves[0], ""); events[len(events)-1].GetPhase() != controlv1.Phase_PHASE_FAILED {
		t.Fatalf("save with no address = %v", events)
	}
	env.fake.addrErr = nil
	env.svc.saveStale(env.ctx, time.Hour)
	if saves := savesOf(env.svc, aliceBox); len(saves) != 1 {
		t.Errorf("scheduler retried a failed save within the interval: %v", saves)
	}
	env.svc.saveStale(env.ctx, time.Nanosecond)
	if saves := savesOf(env.svc, aliceBox); len(saves) != 2 {
		t.Errorf("scheduler started %d saves once the interval passed, want the retry as the 2nd", len(saves))
	}
}

func TestSchedulerSkipsAMachineItsRestoreHasNotFilled(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1")}
	env.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Revision: 1, Meta: map[string]string{metaRestore: snapshotID(1)}})
	env.svc.saveStale(env.ctx, time.Nanosecond)
	if active := env.svc.ops.activeFor(aliceBox); active != nil {
		t.Errorf("scheduler saved a machine its restore has not filled: %s", active.id)
	}
}

func TestRebase(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1"), hexID("c2")}
	op, err := env.svc.Rebase(env.ctx, &controlv1.RebaseRequest{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	newID := env.fake.waitSpec(t, 1).ID
	if !strings.HasPrefix(newID, aliceBox+"-r") {
		t.Fatalf("rebase machine id = %q", newID)
	}
	if pins, _ := env.svc.state.pinned(t.Context(), statePath{user: alice, box: aliceBox}); !pins[snapshotID(1)] {
		t.Errorf("pins while the rebase machine starts = %v, want its snapshot", pins)
	}
	if ep, _, err := env.svc.Resolve(env.ctx, alice, aliceBox); err != nil || ep.Key != aliceBox+":7443" {
		t.Errorf("during rebase Resolve = %q, %v, want the old machine", ep.Key, err)
	}
	env.fake.push(machines.Machine{ID: newID, State: machines.StateRunning})
	events := watch(t, env.svc, env.ctx, op.GetId(), "")
	last := events[len(events)-1]
	if last.GetPhase() != controlv1.Phase_PHASE_DONE || !strings.Contains(last.GetDetail(), "now on "+newID+" (desktop:v2)") {
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
	if spec := env.fake.lastSpec(); spec.Image != "desktop:v2" || spec.Owner != alice || spec.Flavor != defaultFlavor {
		t.Errorf("rebase spec = %+v", spec)
	}
	if c := env.agent.lastPlan().GetConsistency(); c != agentv1.Consistency_CONSISTENCY_QUIESCED {
		t.Errorf("rebase save consistency = %s, want QUIESCED", c)
	}
	restore := env.agent.lastRestore()
	if restore.addr != newID+":7443" || len(restore.gets()) != 2 || restore.reqs[0].GetManifest().GetChunkId() != snapshotID(1) {
		t.Errorf("restore = %s %v", restore.addr, restore.reqs)
	}
	var restored *controlv1.OperationEvent
	for _, ev := range events {
		if ev.GetPhase() == controlv1.Phase_PHASE_RESTORING && ev.GetTotalBytes() > 0 {
			restored = ev
		}
	}
	if restored == nil || restored.GetTotalBytes() != 2<<20 || restored.GetDoneBytes() != 2<<20 {
		t.Errorf("last restore progress = %v, want 2 MiB of 2 MiB plaintext", restored)
	}
	if env.fake.deletedIDs() != aliceBox {
		t.Errorf("deleted = %q, want the old machine", env.fake.deletedIDs())
	}
	if ep, _, err := env.svc.Resolve(env.ctx, alice, aliceBox); err != nil || ep.Key != newID+":7443" {
		t.Errorf("after rebase Resolve = %q, %v, want the new machine", ep.Key, err)
	}
}

func TestRebaseKeepsOldMachineWhenRestoreFails(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1")}
	env.agent.restoreErr = errors.New("disk full")
	op, err := env.svc.Rebase(env.ctx, &controlv1.RebaseRequest{BoxId: aliceBox, Image: "desktop:v3"})
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	newID := env.fake.waitSpec(t, 1).ID
	env.fake.push(machines.Machine{ID: newID, State: machines.StateRunning})
	events := watch(t, env.svc, env.ctx, op.GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_FAILED || last.GetDetail() != "restore failed" {
		t.Errorf("last event = %v", last)
	}
	if env.fake.deletedIDs() != newID {
		t.Errorf("deleted = %q, want only the new machine", env.fake.deletedIDs())
	}
	if ep, _, err := env.svc.Resolve(env.ctx, alice, aliceBox); err != nil || ep.Key != aliceBox+":7443" {
		t.Errorf("Resolve = %q, %v, want the old machine", ep.Key, err)
	}
}

func TestSaveUploadsInBatches(t *testing.T) {
	env := newStateEnv(t)
	env.agent.size = 300 << 20
	env.agent.chunks = []string{hexID("c1"), hexID("c2"), hexID("c3")}
	events := env.save(t)
	uploads := env.agent.allUploads()
	if len(uploads) != 3 {
		t.Fatalf("%d uploads, want 3 batches of 300 MiB", len(uploads))
	}
	for i, up := range uploads {
		if len(up.GetPuts()) != 1 || (up.GetManifest() != nil) != (i == 2) {
			t.Errorf("upload %d: %d puts, manifest %v", i, len(up.GetPuts()), up.GetManifest() != nil)
		}
	}
	var done uint64
	for _, ev := range events {
		if ev.GetTotalBytes() == 0 {
			continue
		}
		if ev.GetTotalBytes() != 900<<20 || ev.GetDoneBytes() < done {
			t.Errorf("progress %d/%d after %d", ev.GetDoneBytes(), ev.GetTotalBytes(), done)
		}
		done = ev.GetDoneBytes()
	}
	if done != 900<<20 {
		t.Errorf("progress ended at %d, want %d", done, 900<<20)
	}

	env.save(t)
	if up := env.agent.lastUpload(); len(up.GetPuts()) != 0 || up.GetManifest() == nil {
		t.Errorf("unchanged save uploaded %d chunks, manifest %v", len(up.GetPuts()), up.GetManifest() != nil)
	}
}

func TestSaveCapsEachUploadAtAPage(t *testing.T) {
	env := newStateEnv(t)
	env.agent.size = 1
	env.agent.chunks = chunkIDs(2*pageSize + 1)
	env.save(t)
	uploads := env.agent.allUploads()
	if len(uploads) != 3 {
		t.Fatalf("%d uploads, want 3", len(uploads))
	}
	sent := map[string]bool{}
	for i, up := range uploads {
		if want := []int{pageSize, pageSize, 1}[i]; len(up.GetPuts()) != want || (up.GetManifest() != nil) != (i == 2) {
			t.Errorf("upload %d: %d puts, manifest %v; want %d puts", i, len(up.GetPuts()), up.GetManifest() != nil, want)
		}
		for _, put := range up.GetPuts() {
			sent[put.GetChunkId()] = true
		}
	}
	if len(sent) != len(env.agent.chunks) {
		t.Errorf("uploaded %d distinct chunks, want %d", len(sent), len(env.agent.chunks))
	}
}

func TestSaveSendsEachAgentCallAFreshCredential(t *testing.T) {
	env := newStateEnv(t)
	env.fake.put(machines.Machine{
		ID: aliceBox, Owner: alice, State: machines.StateRunning, Image: "desktop:v1", Revision: 1,
		Incarnation: "pod0/vm0", Meta: map[string]string{metaWake: "pod0/vm0|w0"},
	})
	signer := &clockSigner{now: time.Unix(1_000_000, 0)}
	env.svc.EnableProvisioning(signer)
	env.agent.size = 300 << 20
	env.agent.chunks = []string{hexID("c1"), hexID("c2"), hexID("c3")}
	var calls []time.Time
	env.agent.onUpload = func(machines.Endpoint) {
		calls = append(calls, signer.read())
		signer.advance(credentialLifetime + time.Minute)
	}
	env.save(t)
	creds := env.agent.uploadCredentials()
	if len(creds) != 3 || len(calls) != 3 {
		t.Fatalf("%d uploads with %d credentials, want 3 batches", len(calls), len(creds))
	}
	for i, cred := range creds {
		if exp := credentialExpiry(t, cred); !exp.After(calls[i]) {
			t.Errorf("upload %d at %v carried a credential that expired at %v", i, calls[i], exp)
		}
	}
}

func TestBatchesSplitByCountAndBytes(t *testing.T) {
	small := make([]chunk, pageSize+1)
	big := []chunk{{Size: 300 << 20}, {Size: 300 << 20}}
	tests := []struct {
		name    string
		missing []chunk
		want    []int
	}{
		{name: "nothing missing", want: []int{0}},
		{name: "one page", missing: small[:pageSize], want: []int{pageSize}},
		{name: "a page and one", missing: small, want: []int{pageSize, 1}},
		{name: "bytes", missing: big, want: []int{1, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []int
			for _, b := range batches(tt.missing) {
				got = append(got, len(b))
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("batch sizes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRestoreSendsChunkURLsInPages(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = chunkIDs(2*pageSize + 1)
	env.save(t)
	env.fake.remove(aliceBox)

	resp, err := env.svc.Ensure(env.ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	env.fake.push(machines.Machine{ID: env.fake.waitSpec(t, 1).ID, State: machines.StateRunning})
	if last := watch(t, env.svc, env.ctx, resp.GetOperation().GetId(), ""); last[len(last)-1].GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("restore ended with %v", last[len(last)-1])
	}
	restore := env.agent.lastRestore()
	first := restore.reqs[0]
	if first.GetKey() == nil || first.GetManifest().GetChunkId() != snapshotID(1) || len(first.GetGets()) != 0 {
		t.Errorf("first message = %v, want the key and manifest only", first)
	}
	var sizes []int
	for _, req := range restore.reqs[1:] {
		sizes = append(sizes, len(req.GetGets()))
		if req.GetKey() != nil || req.GetManifest() != nil {
			t.Errorf("page carries the key or manifest: %v", req)
		}
	}
	if want := []int{pageSize, pageSize, 1}; !slices.Equal(sizes, want) {
		t.Errorf("page sizes = %v, want %v", sizes, want)
	}
	got := map[string]bool{}
	for _, get := range restore.gets() {
		got[get.GetChunkId()] = true
	}
	if len(got) != len(env.agent.chunks) || !got[env.agent.chunks[0]] || !got[env.agent.chunks[2*pageSize]] {
		t.Errorf("restore got URLs for %d distinct chunks, want %d", len(got), len(env.agent.chunks))
	}
}

func TestAPageOfRealURLsFitsTheDefaultGRPCLimit(t *testing.T) {
	const tonicDefault = 4 << 20
	st, err := s3.New(s3.Options{Endpoint: "127.0.0.1:9000", Region: "us-east-1", Bucket: "cocoonbox-state", AccessKey: "ak", SecretKey: "sk", PresignTTL: time.Minute})
	if err != nil {
		t.Fatalf("s3: %v", err)
	}
	agent := newFakeAgent(newFakeStore())
	l := newStateLayer(agent, st, &fakeKeys{})
	ids := chunkIDs(pageSize + 1)
	missing := make([]chunk, len(ids))
	for i, id := range ids {
		missing[i] = chunk{ID: id, Size: 1 << 10}
	}
	key := &agentv1.StateKey{Key: make([]byte, keyring.KeySize), KeyId: "test:" + alice}
	rec := &snapshotRecord{ID: snapshotID(1), UploadedBytes: uint64(len(ids)) << 10}
	if err := l.upload(t.Context(), noAuth, statePath{user: alice, box: aliceBox}, machines.Endpoint{}, rec, key, missing, noProgress); err != nil {
		t.Fatalf("upload: %v", err)
	}
	for i, up := range agent.allUploads() {
		if n := proto.Size(up); n > tonicDefault {
			t.Errorf("upload %d is %d bytes, over %d", i, n, tonicDefault)
		}
	}
	for req, err := range l.restorePages(t.Context(), statePath{user: alice, box: aliceBox}, &agentv1.RestoreRequest{Key: key}, ids) {
		if err != nil {
			t.Fatalf("restore page: %v", err)
		}
		if n := proto.Size(req); n > tonicDefault {
			t.Errorf("restore page of %d gets is %d bytes, over %d", len(req.GetGets()), n, tonicDefault)
		}
	}
}

func TestARestoreThatOutlastsThePresignTTLStillFetchesItsFirstChunks(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = chunkIDs(pageSize + 1)
	env.save(t)
	var skew atomic.Int64
	srv := httptest.NewServer(expiringGets(&skew))
	t.Cleanup(srv.Close)
	presigner, err := s3.New(s3.Options{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Region: "us-east-1", Bucket: "cocoonbox-state", AccessKey: "ak", SecretKey: "sk", PresignTTL: time.Minute})
	if err != nil {
		t.Fatalf("s3: %v", err)
	}
	agent := &slowAgent{fakeAgent: env.agent, skew: &skew, perPage: 2 * time.Minute}
	l := newStateLayer(agent, &presignedStore{fakeStore: env.store, presigner: presigner}, env.keys)
	ctx, cancel := context.WithTimeout(t.Context(), stateTimeout)
	defer cancel()

	p := statePath{user: alice, box: aliceBox}
	if _, err := l.restore(ctx, p, machines.Endpoint{}, headID(t, l, p), noProgress); err != nil {
		t.Fatalf("restore over %s with a %s presign TTL: %v", time.Duration(skew.Load()), time.Minute, err)
	}
	if want := pageSize + 2; agent.fetched != want {
		t.Errorf("agent fetched %d URLs, want %d", agent.fetched, want)
	}
}

func TestNewKeyReuploadsEverything(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1"), hexID("c2")}
	env.save(t)
	env.keys.id = "rotated"
	env.save(t)
	if n := len(env.agent.lastUpload().GetPuts()); n != 2 {
		t.Errorf("save under a new key uploaded %d chunks, want 2", n)
	}
}

func TestSaveRefusesUnsafeIDs(t *testing.T) {
	tests := []struct {
		name     string
		snapshot string
		chunks   []string
	}{
		{name: "snapshot id escapes the box prefix", snapshot: "../cb-000000000000/head", chunks: []string{hexID("c1")}},
		{name: "chunk id escapes the box prefix", chunks: []string{"../../cb-000000000000/head.json"}},
		{name: "chunk id is not hex", chunks: []string{strings.Repeat("Z", 64)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newStateEnv(t)
			env.agent.snapshotID, env.agent.chunks = tt.snapshot, tt.chunks
			op, err := env.svc.SaveState(env.ctx, &controlv1.BoxRef{BoxId: aliceBox})
			if err != nil {
				t.Fatalf("SaveState: %v", err)
			}
			events := watch(t, env.svc, env.ctx, op.GetId(), "")
			if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_FAILED || last.GetDetail() != "save failed" {
				t.Errorf("last event = %v, want FAILED with an opaque detail", last)
			}
			if n := env.agent.uploadCount(); n != 0 {
				t.Errorf("agent received %d uploads", n)
			}
			if len(env.store.objects) != 0 {
				t.Errorf("store written: %v", env.store.objects)
			}
		})
	}
}

func TestUpgradeWithoutAnImageStaysInTheBoxFamily(t *testing.T) {
	fake, svc, ctx := newFamilyEnv(t, "cbcore:v1", "cbdesk:v25", "cbcore:v1", "cbdesk:v24")
	if _, err := svc.Rebase(ctx, &controlv1.RebaseRequest{BoxId: aliceBox}); err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if spec := fake.waitSpec(t, 1); spec.Image != "cbcore:v1" {
		t.Errorf("upgrade target = %q, want cbcore:v1, not the cbdesk default", spec.Image)
	}
}

func TestRebaseStaysInTheBoxFamilyUnlessTheOperatorAsks(t *testing.T) {
	fake, svc, ctx := newFamilyEnv(t, "cbdroid:v3", "cbcore:v2", "cbcore:v1")
	if _, err := svc.Rebase(ctx, &controlv1.RebaseRequest{BoxId: aliceBox}); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "choose cbcore:v2, cbcore:v1") {
		t.Errorf("Rebase without an image off an offered family = %v, want InvalidArgument naming the offered images", err)
	}
	if _, err := svc.Rebase(ctx, &controlv1.RebaseRequest{BoxId: aliceBox, Image: "cbcore:v2"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Rebase of a user to another family = %v, want FailedPrecondition", err)
	}
	if n := fake.specCount(); n != 0 {
		t.Fatalf("refused Rebases created %d machines", n)
	}
	operatorCtx := metadata.NewIncomingContext(t.Context(), metadata.Pairs(auth.HeaderOperatorUser, alice))
	_, err := auth.OperatorUnary(operatorCtx, &controlv1.RebaseRequest{BoxId: aliceBox, Image: "cbcore:v2"}, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) {
		return svc.Rebase(ctx, req.(*controlv1.RebaseRequest))
	})
	if err != nil {
		t.Fatalf("operator Rebase to another family: %v", err)
	}
	if spec := fake.waitSpec(t, 1); spec.Image != "cbcore:v2" {
		t.Errorf("operator Rebase target = %q, want cbcore:v2", spec.Image)
	}
}

func TestAnOperatorRebaseToAFlavorResizesTheBoxOnlyWhenItsFilesFit(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1")}
	rebase := func(flavor string) (*controlv1.Operation, error) {
		ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs(auth.HeaderOperatorUser, alice, auth.HeaderOperatorFlavor, flavor))
		op, err := auth.OperatorUnary(ctx, &controlv1.RebaseRequest{BoxId: aliceBox}, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) {
			return env.svc.Rebase(ctx, req.(*controlv1.RebaseRequest))
		})
		if err != nil {
			return nil, err
		}
		return op.(*controlv1.Operation), nil
	}
	if _, err := rebase("2/4096"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("operator Rebase to a malformed flavor = %v, want InvalidArgument", err)
	}
	op, err := rebase("2/4096/2")
	if err != nil {
		t.Fatalf("operator Rebase to a 2 GiB disk: %v", err)
	}
	if events := watch(t, env.svc, env.ctx, op.GetId(), ""); events[len(events)-1].GetPhase() != controlv1.Phase_PHASE_FAILED || !strings.Contains(events[len(events)-1].GetDetail(), "2 GiB disk") {
		t.Fatalf("Rebase onto a disk the files do not fit = %v, want it refused", events[len(events)-1])
	}
	if n := env.fake.specCount(); n != 0 {
		t.Errorf("a refused resize created %d machines", n)
	}
	if flavor, _ := env.svc.flavorOf(alice, aliceBox); flavor != defaultFlavor {
		t.Errorf("box flavor after a refused resize = %+v, want %+v", flavor, defaultFlavor)
	}
	small := machines.Flavor{VCPUs: 2, MemoryMiB: 4096, DiskGiB: 20}
	if op, err = rebase("2/4096/20"); err != nil {
		t.Fatalf("operator Rebase to 2/4096/20: %v", err)
	}
	created := env.fake.waitSpec(t, 1)
	if created.Flavor != small {
		t.Errorf("resized machine flavor = %+v, want %+v", created.Flavor, small)
	}
	if flavor, _ := env.svc.flavorOf(alice, aliceBox); flavor != defaultFlavor {
		t.Errorf("box flavor before the new machine serves = %+v, want the old one", flavor)
	}
	env.fake.push(machines.Machine{ID: created.ID, State: machines.StateRunning})
	if events := watch(t, env.svc, env.ctx, op.GetId(), ""); events[len(events)-1].GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("resize = %v", events[len(events)-1])
	}
	if flavor, _ := env.svc.flavorOf(alice, aliceBox); flavor != small {
		t.Errorf("box flavor after the resize = %+v, want %+v", flavor, small)
	}
}

func TestAResizeWhoseFlavorWriteFailsKeepsTheOldMachineAndFlavor(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1")}
	env.svc.registry.(*fakeRegistry).flavorErr = errors.New("database is restarting")
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs(auth.HeaderOperatorUser, alice, auth.HeaderOperatorFlavor, "2/4096/20"))
	got, err := auth.OperatorUnary(ctx, &controlv1.RebaseRequest{BoxId: aliceBox}, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) {
		return env.svc.Rebase(ctx, req.(*controlv1.RebaseRequest))
	})
	if err != nil {
		t.Fatalf("operator Rebase to 2/4096/20: %v", err)
	}
	created := env.fake.waitSpec(t, 1)
	env.fake.push(machines.Machine{ID: created.ID, State: machines.StateRunning})
	last := watch(t, env.svc, env.ctx, got.(*controlv1.Operation).GetId(), "")
	if ev := last[len(last)-1]; ev.GetPhase() != controlv1.Phase_PHASE_FAILED || !strings.Contains(ev.GetDetail(), "the old machine stays") {
		t.Fatalf("resize with a failing flavor write = %v, want it failed", ev)
	}
	if deleted := env.fake.deletedIDs(); deleted != created.ID {
		t.Errorf("deleted machines = %q, want only the new %s", deleted, created.ID)
	}
	if flavor, _ := env.svc.flavorOf(alice, aliceBox); flavor != defaultFlavor {
		t.Errorf("box flavor = %+v, want %+v", flavor, defaultFlavor)
	}
}

func TestRebaseRefusesImagesOffTheAllowList(t *testing.T) {
	env := newStateEnv(t)
	for _, image := range []string{"vk-cocoonbox-cb-9f86d081884c-0-984b48", "cocoonbox-desktop-v9", "desktop:v1"} {
		_, err := env.svc.Rebase(env.ctx, &controlv1.RebaseRequest{BoxId: aliceBox, Image: image})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("Rebase to %q: %v, want InvalidArgument", image, err)
		}
	}
	if n := env.fake.specCount(); n != 0 {
		t.Errorf("Rebase created %d machines", n)
	}
	if n := env.agent.plans(); n != 0 {
		t.Errorf("Rebase planned %d snapshots", n)
	}
}

func TestEnsureRestoresAMissingMachineFromTheLatestSnapshot(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1"), hexID("c2")}
	env.save(t)
	env.fake.remove(aliceBox)

	resp, err := env.svc.Ensure(env.ctx, &controlv1.EnsureRequest{})
	if err != nil || resp.GetOperation().GetId() == "" || resp.GetBox().GetState() != controlv1.BoxState_BOX_STATE_STARTING {
		t.Fatalf("Ensure = %v, %v", resp, err)
	}
	spec := env.fake.waitSpec(t, 1)
	if !strings.HasPrefix(spec.ID, aliceBox+"-r") || spec.Owner != alice {
		t.Errorf("restore machine spec = %+v, want a new %s-r machine", spec, aliceBox)
	}
	newID := spec.ID
	again, err := env.svc.Ensure(env.ctx, &controlv1.EnsureRequest{})
	if err != nil || again.GetOperation().GetId() != resp.GetOperation().GetId() || env.fake.specCount() != 1 {
		t.Errorf("second Ensure = %v, %v, %d specs, want the running restore", again, err, env.fake.specCount())
	}
	if _, err := env.svc.Hibernate(env.ctx, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Hibernate during the restore = %v, want FailedPrecondition", err)
	}
	env.fake.push(machines.Machine{ID: newID, State: machines.StateRunning})
	events := watch(t, env.svc, env.ctx, resp.GetOperation().GetId(), "")
	last := events[len(events)-1]
	if last.GetPhase() != controlv1.Phase_PHASE_DONE || !strings.Contains(last.GetDetail(), "(snapshot "+snapshotID(1)) ||
		!strings.Contains(last.GetDetail(), "restored the state saved at ") || !strings.Contains(last.GetDetail(), "anything written after that save is not in this box") ||
		strings.Contains(last.GetDetail(), "lost") {
		t.Fatalf("last event = %v", last)
	}
	if !slices.ContainsFunc(events, func(ev *controlv1.OperationEvent) bool {
		return ev.GetPhase() == controlv1.Phase_PHASE_RESTORING && ev.GetTotalBytes() > 0
	}) {
		t.Errorf("no restore progress in %v", events)
	}
	restore := env.agent.lastRestore()
	if restore.addr != newID+":7443" || restore.reqs[0].GetManifest().GetChunkId() != snapshotID(1) || len(restore.gets()) != 2 {
		t.Errorf("restore = %s %v", restore.addr, restore.reqs)
	}
	if ep, _, err := env.svc.Resolve(env.ctx, alice, aliceBox); err != nil || ep.Key != newID+":7443" {
		t.Errorf("Resolve after the restore = %q, %v", ep.Key, err)
	}
	if env.fake.deletedIDs() != "" {
		t.Errorf("deleted %q after a good restore", env.fake.deletedIDs())
	}
}

func TestEnsureRestoresAMissingMachineIntoItsImageFamily(t *testing.T) {
	tests := []struct {
		name, image               string
		legacyHead, parked, owner bool
		want                      string
	}{
		{"cbcore box", "cbcore:v6", false, false, false, "cbcore:v7"},
		{"cbdesk box", "cbdesk:v22", false, false, false, "cbdesk:v25"},
		{"head without an image", "cbcore:v6", true, false, false, "cbdesk:v25"},
		{"parked box on an offered image", "cbcore:v6", false, true, false, "cbcore:v6"},
		{"parked box on a retired image", "cbcore:v5", false, true, false, "cbcore:v7"},
		{"image owner's cbcore box", "cbcore:v6", false, false, true, "cbcore:v8"},
		{"image owner's parked box on an offered image", "cbcore:v6", false, true, true, "cbcore:v6"},
		{"image owner's parked box on a retired image", "cbcore:v5", false, true, true, "cbcore:v8"},
		{"box of a family not offered", "cbdroid:v3", false, false, true, "cbdroid:v3"},
		{"parked box of a family not offered", "cbdroid:v3", false, true, true, "cbdroid:v3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newStateEnv(t)
			env.svc.defaults.Image, env.svc.defaults.RebaseImages = "cbdesk:v25", []string{"cbcore:v7", "cbcore:v6", "cbdesk:v24", "cbcore:v8"}
			if tt.owner {
				env.svc.registry.(*fakeRegistry).setPolicy(alice, users.Policy{Image: "cbcore:v8"})
			}
			env.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Image: tt.image, Revision: 1})
			env.agent.chunks = []string{hexID("c1")}
			env.save(t)
			p := statePath{user: alice, box: aliceBox}
			if tt.legacyHead || tt.parked {
				h := head{Latest: snapshotID(1), SavedAtMs: 100}
				if tt.parked {
					h.Image, h.Parked = tt.image, true
				}
				if err := putJSON(t.Context(), env.store, p.head(), h); err != nil {
					t.Fatalf("setup: %v", err)
				}
				env.svc.state.heads = map[statePath]head{}
			}
			env.fake.remove(aliceBox)

			if _, err := env.svc.Ensure(env.ctx, &controlv1.EnsureRequest{}); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if spec := env.fake.waitSpec(t, 1); spec.Image != tt.want {
				t.Errorf("restore machine image = %q, want %q", spec.Image, tt.want)
			}
		})
	}
}

func TestNoClientReachesAMachineWhileItIsRestored(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1")}
	env.save(t)
	env.fake.remove(aliceBox)
	block := make(chan struct{})
	env.agent.restoreGate = block

	resp, err := env.svc.Ensure(env.ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	env.fake.push(machines.Machine{ID: env.fake.waitSpec(t, 1).ID, State: machines.StateRunning})
	clustertest.WaitFor(t, "the restore reaches the agent", func() bool { return env.agent.restoreCount() > 0 })
	if _, _, err := env.svc.Resolve(env.ctx, alice, aliceBox); status.Code(err) != codes.Unavailable {
		t.Errorf("Resolve while restoring = %v, want Unavailable", err)
	}
	if box, _ := env.svc.get(env.ctx, aliceBox); box.GetState() != controlv1.BoxState_BOX_STATE_STARTING {
		t.Errorf("box while restoring = %v, want STARTING", box)
	}
	if _, err := env.svc.SaveState(env.ctx, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SaveState while restoring = %v, want FailedPrecondition", err)
	}
	plans := env.agent.plans()
	env.svc.saveStale(env.ctx, time.Nanosecond)
	if env.agent.plans() != plans {
		t.Error("the scheduler saved a box during its restore")
	}
	close(block)
	if last := watch(t, env.svc, env.ctx, resp.GetOperation().GetId(), ""); last[len(last)-1].GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("restore ended with %v", last[len(last)-1])
	}
}

func TestAFailedRestoreDeletesTheMachineSoEnsureRetries(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1")}
	env.save(t)
	env.fake.remove(aliceBox)
	env.agent.restoreErr = errors.New("disk full")

	resp, err := env.svc.Ensure(env.ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	first := env.fake.waitSpec(t, 1).ID
	env.fake.push(machines.Machine{ID: first, State: machines.StateRunning})
	events := watch(t, env.svc, env.ctx, resp.GetOperation().GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_FAILED || last.GetDetail() != "restore failed" {
		t.Fatalf("last event = %v", last)
	}
	if env.fake.deletedIDs() != first {
		t.Errorf("deleted %q, want the empty machine %s", env.fake.deletedIDs(), first)
	}
	env.agent.restoreErr = nil
	retry, err := env.svc.Ensure(env.ctx, &controlv1.EnsureRequest{})
	if err != nil || retry.GetOperation().GetId() == resp.GetOperation().GetId() || env.fake.specCount() != 2 {
		t.Fatalf("retry Ensure = %v, %v, %d specs", retry, err, env.fake.specCount())
	}
	env.fake.push(machines.Machine{ID: env.fake.waitSpec(t, 2).ID, State: machines.StateRunning})
	if last := watch(t, env.svc, env.ctx, retry.GetOperation().GetId(), ""); last[len(last)-1].GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Errorf("retried restore ended with %v", last[len(last)-1])
	}
}

func TestARestoreCutShortByARestartRestoresAgain(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1")}
	env.save(t)
	env.fake.remove(aliceBox)
	block := make(chan struct{})
	env.agent.restoreGate = block
	cut, err := env.svc.Ensure(env.ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	first := env.fake.waitSpec(t, 1).ID
	env.fake.push(machines.Machine{ID: first, State: machines.StateRunning})
	clustertest.WaitFor(t, "the restore reaches the agent", func() bool { return env.agent.restoreCount() > 0 })

	restarted := newService(env.fake, testDefaults, env.svc.registry)
	restarted.EnableState(env.agent, env.store, env.keys)
	if _, _, err := restarted.Resolve(env.ctx, alice, aliceBox); status.Code(err) != codes.Unavailable {
		t.Errorf("Resolve of the unrestored machine after a restart = %v, want Unavailable", err)
	}
	resp, err := restarted.Ensure(env.ctx, &controlv1.EnsureRequest{})
	if err != nil || resp.GetOperation().GetId() == "" || env.fake.deletedIDs() != first {
		t.Fatalf("Ensure after a restart = %v, %v, deleted %q; want %s discarded and a new restore", resp, err, env.fake.deletedIDs(), first)
	}
	close(block)
	watch(t, env.svc, env.ctx, cut.GetOperation().GetId(), "")
	env.fake.push(machines.Machine{ID: env.fake.waitSpec(t, 2).ID, State: machines.StateRunning})
	if last := watch(t, restarted, env.ctx, resp.GetOperation().GetId(), ""); last[len(last)-1].GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("restore after the restart ended with %v", last[len(last)-1])
	}
	if _, _, err := restarted.Resolve(env.ctx, alice, aliceBox); err != nil {
		t.Errorf("Resolve after the restore = %v", err)
	}
}

func TestEnsureWithoutSavedStateCreatesAnEmptyBox(t *testing.T) {
	env := newStateEnv(t)
	env.fake.remove(aliceBox)

	resp, err := env.svc.Ensure(env.ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	env.fake.push(machines.Machine{ID: aliceBox, State: machines.StateRunning})
	events := watch(t, env.svc, env.ctx, resp.GetOperation().GetId(), "")
	restored := slices.ContainsFunc(events, func(ev *controlv1.OperationEvent) bool { return ev.GetPhase() == controlv1.Phase_PHASE_RESTORING })
	if restored || events[len(events)-1].GetPhase() != controlv1.Phase_PHASE_DONE || env.agent.restoreCount() != 0 {
		t.Errorf("events = %v, restores %d, want a plain create", events, env.agent.restoreCount())
	}
}

func TestCommitRetriesAFailedStoreWrite(t *testing.T) {
	env := newStateEnv(t)
	env.svc.state.retryDelay = time.Millisecond
	index := alice + "/index.json"
	env.agent.chunks = []string{hexID("c1")}
	env.save(t)
	before := env.store.putCount(index)
	env.agent.chunks = []string{hexID("c1"), hexID("c2")}
	env.store.failNext(index, 1)
	env.save(t)
	if n := env.store.putCount(index) - before; n != 2 {
		t.Errorf("index written %d times, want a failure and a retry", n)
	}
	if idx := string(env.store.objects[index]); !strings.Contains(idx, hexID("c1")) || !strings.Contains(idx, hexID("c2")) {
		t.Errorf("index = %s", idx)
	}
	env.agent.chunks = append(env.agent.chunks, hexID("c3"))
	env.save(t)
	if puts := env.agent.lastUpload().GetPuts(); len(puts) != 1 || puts[0].GetChunkId() != hexID("c3") {
		t.Errorf("next upload = %v, want only c3", puts)
	}
}

func TestSaveHealsAfterACommitThatKeptFailing(t *testing.T) {
	env := newStateEnv(t)
	env.svc.state.retryDelay = time.Millisecond
	env.agent.chunks = []string{hexID("c1"), hexID("c2")}
	env.save(t)
	env.agent.chunks = append(env.agent.chunks, hexID("c3"))
	env.store.failNext(alice+"/index.json", commitAttempts)
	op, err := env.svc.SaveState(env.ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if events := watch(t, env.svc, env.ctx, op.GetId(), ""); events[len(events)-1].GetPhase() != controlv1.Phase_PHASE_FAILED {
		t.Fatalf("save with a failing commit ended with %v", events[len(events)-1])
	}

	env.save(t)
	plans := env.agent.allPlans()
	if got := []string{plans[2].GetParent(), plans[3].GetParent()}; !slices.Equal(got, []string{snapshotID(1), ""}) {
		t.Errorf("parents of the plans after the failed commit = %q, want the old head, then none", got)
	}
	rec, err := env.svc.state.readRecord(env.ctx, statePath{user: alice, box: aliceBox}, snapshotID(4), nil)
	if err != nil || rec.Parent != snapshotID(1) || len(rec.Chunks) != 3 {
		t.Errorf("healed snapshot = %+v, %v", rec, err)
	}
	env.save(t)
	if plan, up := env.agent.lastPlan(), env.agent.lastUpload(); plan.GetParent() != snapshotID(4) || len(up.GetPuts()) != 0 {
		t.Errorf("save after healing planned on %q and uploaded %d chunks", plan.GetParent(), len(up.GetPuts()))
	}
}

func TestBoxesOfOneUserShareTheirChunksAndIndex(t *testing.T) {
	st := newFakeStore()
	agent := newFakeAgent(st)
	agent.byBox = map[string][]string{
		"a": {hexID("c1"), hexID("c2")},
		"b": {hexID("c2"), hexID("c3")},
		"c": {hexID("c1"), hexID("c3")},
	}
	l := newStateLayer(agent, st, &fakeKeys{})
	boxA, boxB := statePath{user: alice, box: "cb-00000000000a"}, statePath{user: alice, box: "cb-00000000000b"}
	var inner error
	agent.onUpload = func(ep machines.Endpoint) {
		if ep.Key == "a" {
			agent.onUpload = nil
			_, inner = l.save(t.Context(), noAuth, boxB, machines.Endpoint{Key: "b"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress)
		}
	}
	if _, err := l.save(t.Context(), noAuth, boxA, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress); err != nil || inner != nil {
		t.Fatalf("save a: %v; save b during a's upload: %v", err, inner)
	}
	var idx chunkIndex
	if err := getJSON(t.Context(), st, alice+"/index.json", &idx); err != nil || len(idx.Chunks) != 3 {
		t.Errorf("index after parallel saves = %v, %v, want c1, c2 and c3", idx.Chunks, err)
	}
	for _, key := range []string{boxA.head(), boxB.head(), boxA.chunk(hexID("c3")), boxB.manifest(snapshotID(2))} {
		if !st.has(key) {
			t.Errorf("store lacks %s", key)
		}
	}
	saveLive(t, l, statePath{user: alice, box: "cb-00000000000c"}, "c")
	if n := len(agent.lastUpload().GetPuts()); n != 0 {
		t.Errorf("a third box with chunks the user holds uploaded %d", n)
	}
}

func TestTwoLayersOnAVersionedStoreMergeTheirIndexWrites(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	agent.byBox = map[string][]string{"a": hexIDs("c1", "c2"), "b": hexIDs("c2", "c3")}
	la, lb := st.layer(agent), st.layer(agent)
	boxA, boxB := statePath{user: alice, box: "cb-00000000000a"}, statePath{user: alice, box: "cb-00000000000b"}
	var inner error
	agent.onUpload = func(ep machines.Endpoint) {
		if ep.Key == "a" {
			agent.onUpload = nil
			_, inner = lb.save(t.Context(), noAuth, boxB, machines.Endpoint{Key: "b"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress)
		}
	}
	if _, err := la.save(t.Context(), noAuth, boxA, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress); err != nil || inner != nil {
		t.Fatalf("save a: %v; save b on another layer during a's upload: %v", err, inner)
	}
	var idx chunkIndex
	if err := getJSON(t.Context(), st, alice+"/index.json", &idx); err != nil || len(idx.Chunks) != 3 {
		t.Errorf("index after saves on two layers = %v, %v, want c1, c2 and c3", idx.Chunks, err)
	}
}

func TestTheGCFailsWhenAnotherLayersSaveWroteTheIndexAfterItsRead(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	saver, collector := st.layer(agent), st.layer(agent)
	p := statePath{user: alice, box: "cb-00000000000a"}
	for _, chunks := range [][]string{{"c1", "c2"}, {"c2", "c3"}} {
		agent.byBox = map[string][]string{"a": hexIDs(chunks...)}
		saveLive(t, saver, p, "a")
	}
	saved := false
	st.before = func(op, key string) {
		if op != "putif" || key != p.index() || saved {
			return
		}
		saved = true
		agent.byBox = map[string][]string{"a": hexIDs("c1", "c3")}
		if _, err := saver.save(t.Context(), noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress); err != nil {
			t.Errorf("save during the gc sweep: %v", err)
		}
	}
	_, err := collector.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now().Add(40*24*time.Hour), everyBoxLive)
	if !errors.Is(err, store.ErrStale) {
		t.Fatalf("collect across a save = %v, want ErrStale", err)
	}
	if !st.has(p.chunk(hexID("c1"))) {
		t.Error("the gc deleted c1, which a save on another layer committed during its sweep")
	}
}

func TestAGCPassBumpsTheIndexGenerationAndFailsASavePlannedBeforeIt(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	saver, collector := st.layer(agent), st.layer(agent)
	p := statePath{user: alice, box: "cb-00000000000a"}
	for _, chunks := range [][]string{{"c1", "c2"}, {"c2", "c3"}} {
		agent.byBox = map[string][]string{"a": hexIDs(chunks...)}
		saveLive(t, saver, p, "a")
	}
	agent.byBox = map[string][]string{"a": hexIDs("c1", "c3")}
	var collected error
	agent.onUpload = func(machines.Endpoint) {
		agent.onUpload = nil
		_, collected = collector.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now().Add(40*24*time.Hour), everyBoxLive)
	}
	_, err := saver.save(t.Context(), noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress)
	if collected != nil {
		t.Fatalf("collect during the save: %v", collected)
	}
	var idx chunkIndex
	if gerr := getJSON(t.Context(), st, p.index(), &idx); gerr != nil || idx.Generation != 1 {
		t.Fatalf("index after the gc pass = %+v, %v, want generation 1", idx, gerr)
	}
	if !errors.Is(err, errIndexCollected) {
		t.Errorf("save planned before the gc pass = %v, want errIndexCollected", err)
	}
	if h, err := collector.head(t.Context(), p); err != nil || h.Latest != snapshotID(2) {
		t.Errorf("head after the failed save = %+v, %v, want the second snapshot", h, err)
	}
}

func TestAGCPassThatDeletesAnUnindexedUploadFailsTheSaveThatUploadedIt(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	saver, collector := st.layer(agent), st.layer(agent)
	p := statePath{user: alice, box: "cb-00000000000a"}
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	saveLive(t, saver, p, "a")
	agent.byBox = map[string][]string{"a": hexIDs("c1", "c2")}
	var collected error
	ran := false
	st.before = func(op, key string) {
		if op != "put" || key != p.manifest(snapshotID(2)) || ran {
			return
		}
		ran = true
		_, collected = collector.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now().Add(2*gcGrace), everyBoxLive)
	}
	_, err := saver.save(t.Context(), noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress)
	if !ran || collected != nil || st.has(p.chunk(hexID("c2"))) {
		t.Fatalf("collect after the upload ran %t, %v, c2 left %t; want it to delete the unindexed c2", ran, collected, st.has(p.chunk(hexID("c2"))))
	}
	if !errors.Is(err, errIndexCollected) {
		t.Errorf("save whose upload the gc deleted = %v, want errIndexCollected", err)
	}
}

func TestAHeadReadBeforeASaveNeverReplacesTheHeadTheSaveCached(t *testing.T) {
	st := newFakeStore()
	agent := newFakeAgent(st)
	p := statePath{user: alice, box: "cb-00000000000a"}
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	saveLive(t, newStateLayer(agent, st, &fakeKeys{}), p, "a")
	slow := &afterGetStore{fakeStore: st}
	l := newStateLayer(agent, slow, &fakeKeys{})
	agent.byBox = map[string][]string{"a": hexIDs("c1", "c2")}
	slow.after = func(key string) {
		if key != p.head() {
			return
		}
		slow.after = nil
		if _, err := l.save(t.Context(), noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress); err != nil {
			t.Errorf("save during the head read: %v", err)
		}
	}
	if h, err := l.head(t.Context(), p); err != nil || h.Latest != snapshotID(1) {
		t.Fatalf("head read before the save = %+v, %v", h, err)
	}
	if h, _ := l.head(t.Context(), p); h.Latest != snapshotID(2) {
		t.Errorf("cached head %q; want the save's head kept over the older read", h.Latest)
	}
}

func TestTheGCKeepsTheChunksAndRecordsASaveWroteWithinItsGrace(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	saver, collector := st.layer(agent), st.layer(agent)
	p := statePath{user: alice, box: "cb-00000000000a"}
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	saveLive(t, saver, p, "a")
	st.mu.Lock()
	for key := range st.modified {
		st.modified[key] = time.Now().Add(-40 * 24 * time.Hour)
	}
	st.mu.Unlock()
	agent.byBox = map[string][]string{"a": hexIDs("c1", "c2")}
	var r gcReport
	var collected error
	st.before = func(op, key string) {
		if op != "put" || key != p.record(snapshotID(2)) {
			return
		}
		st.before = nil
		r, collected = collector.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now(), everyBoxLive)
	}
	saveLive(t, saver, p, "a")
	if collected != nil || r.chunks != 0 || !st.has(p.chunk(hexID("c2"))) || !st.has(p.manifest(snapshotID(2))) {
		t.Errorf("the gc pass during a save = %+v, %v; want the save's fresh chunk and manifest kept", r, collected)
	}
}

func TestTheGCKeepsAnOldChunkASaveReusesBeforeItsTimelineNamesIt(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	saver, collector := st.layer(agent), st.layer(agent)
	p := statePath{user: alice, box: "cb-00000000000a"}
	for _, chunks := range [][]string{{"c1"}, {"c2"}} {
		agent.byBox = map[string][]string{"a": hexIDs(chunks...)}
		saveLive(t, saver, p, "a")
	}
	if _, err := collector.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now(), everyBoxLive); err != nil {
		t.Fatalf("collect that writes the timeline: %v", err)
	}
	tl, _, err := saver.readTimeline(t.Context(), p)
	if err != nil || len(tl.Snapshots) != 2 {
		t.Fatalf("readTimeline = %+v, %v", tl, err)
	}
	if err := putJSON(t.Context(), st, p.timeline(), timeline{Snapshots: tl.Snapshots[1:]}); err != nil {
		t.Fatalf("drop the first snapshot from the timeline: %v", err)
	}
	if err := st.Delete(t.Context(), []string{p.record(snapshotID(1)), p.manifest(snapshotID(1))}); err != nil {
		t.Fatalf("delete the first snapshot: %v", err)
	}
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	st.mu.Lock()
	for key := range st.modified {
		st.modified[key] = time.Now().Add(-40 * 24 * time.Hour)
	}
	st.mu.Unlock()
	var collected error
	ran := false
	st.before = func(op, key string) {
		if op != "putif" || key != p.timeline() || ran {
			return
		}
		ran = true
		_, collected = collector.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now(), everyBoxLive)
	}
	_, err = saver.save(t.Context(), noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress)
	if !ran || collected != nil {
		t.Fatalf("collect before the timeline write ran %t, %v", ran, collected)
	}
	if err != nil || !st.has(p.chunk(hexID("c1"))) {
		t.Errorf("save over a gc pass = %v, c1 kept %t; want the save committed and the chunk it reused kept", err, st.has(p.chunk(hexID("c1"))))
	}
}

func TestPruneWritesTheTimelineOnlyOnTheVersionItRead(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	saver, collector := st.layer(agent), st.layer(agent)
	p := statePath{user: alice, box: "cb-00000000000a"}
	save := func(chunks ...string) {
		t.Helper()
		agent.byBox = map[string][]string{"a": hexIDs(chunks...)}
		saveLive(t, saver, p, "a")
	}
	save("c1")
	if _, err := collector.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now(), everyBoxLive); err != nil {
		t.Fatalf("collect that writes the timeline: %v", err)
	}
	save("c2")
	saved := false
	st.before = func(op, key string) {
		if op != "putif" || key != p.timeline() || saved {
			return
		}
		saved = true
		save("c3")
	}
	if _, err := st.layer(agent).collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now().Add(40*24*time.Hour), everyBoxLive); !errors.Is(err, store.ErrStale) {
		t.Fatalf("collect across a timeline append = %v, want ErrStale", err)
	}
	if !st.has(p.record(snapshotID(1))) {
		t.Error("the gc deleted a record after its timeline write lost")
	}
}

func TestNoMoreThanSaveFanoutSavesOfAProcessPlanAndUploadAtOnce(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	layer := st.layer(agent)
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	gate := make(chan struct{})
	var uploading atomic.Int32
	agent.onUpload = func(machines.Endpoint) {
		uploading.Add(1)
		<-gate
	}
	done := make(chan error, saveFanout+1)
	for i := range saveFanout + 1 {
		p := statePath{user: alice, box: fmt.Sprintf("cb-fanout-%d", i)}
		go func() {
			_, err := layer.save(t.Context(), noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress)
			done <- err
		}()
	}
	clustertest.WaitFor(t, "saveFanout uploads", func() bool { return uploading.Load() == saveFanout })
	time.Sleep(100 * time.Millisecond)
	agent.mu.Lock()
	plans := len(agent.planReqs)
	agent.mu.Unlock()
	if got := uploading.Load(); got != saveFanout || plans != saveFanout {
		t.Errorf("saves planning or uploading at once = %d uploads, %d plans; want %d of %d", got, plans, saveFanout, saveFanout+1)
	}
	close(gate)
	for range saveFanout + 1 {
		if err := <-done; err != nil {
			t.Errorf("save: %v", err)
		}
	}
}

type stateEnv struct {
	ctx   context.Context
	svc   *Service
	fake  *fakeMachines
	agent *fakeAgent
	store *fakeStore
	keys  *fakeKeys
}

func newStateEnv(t *testing.T) *stateEnv {
	t.Helper()
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Image: "desktop:v1", Revision: 1})
	st := newFakeStore()
	agent := newFakeAgent(st)
	keys := &fakeKeys{}
	svc := newService(fake, testDefaults, newFakeRegistry())
	svc.EnableState(agent, st, keys)
	return &stateEnv{ctx: auth.WithUser(t.Context(), alice), svc: svc, fake: fake, agent: agent, store: st, keys: keys}
}

func (e *stateEnv) save(t *testing.T) []*controlv1.OperationEvent {
	t.Helper()
	events := e.saveBox(t, aliceBox)
	time.Sleep(time.Millisecond)
	return events
}

func (e *stateEnv) saveEach(t *testing.T, chunks ...[]string) {
	t.Helper()
	for _, c := range chunks {
		e.agent.chunks = hexIDs(c...)
		e.save(t)
	}
}

func (e *stateEnv) saveBox(t *testing.T, box string) []*controlv1.OperationEvent {
	t.Helper()
	op, err := e.svc.SaveState(e.ctx, &controlv1.BoxRef{BoxId: box})
	return e.finish(t, "save of "+box, op, err)
}

func (e *stateEnv) deleteBox(t *testing.T, box string) {
	t.Helper()
	op, err := e.svc.Delete(e.ctx, &controlv1.BoxRef{BoxId: box})
	e.finish(t, "delete of "+box, op, err)
}

func (e *stateEnv) finish(t *testing.T, what string, op *controlv1.Operation, err error) []*controlv1.OperationEvent {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	events := watch(t, e.svc, e.ctx, op.GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("%s ended with %v", what, last)
	}
	return events
}

func (e *stateEnv) trySave() error {
	op, err := e.svc.SaveState(e.ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		return err
	}
	for {
		events, done, changed := e.svc.ops.local(op.GetId()).since(0)
		if done {
			if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_DONE {
				return fmt.Errorf("save ended with %v", last)
			}
			return nil
		}
		<-changed
	}
}

func (e *stateEnv) addBox(t *testing.T, box string) {
	t.Helper()
	if _, err := e.svc.registry.AddBox(e.ctx, users.Box{ID: box, Owner: alice, Name: box}); err != nil {
		t.Fatalf("AddBox: %v", err)
	}
	e.fake.put(machines.Machine{ID: box, Owner: alice, State: machines.StateRunning, Image: "desktop:v1", Revision: 1})
}

func (e *stateEnv) collect(t *testing.T, opts GCOptions, ahead time.Duration) gcReport {
	t.Helper()
	r, err := e.svc.state.collect(e.ctx, alice, opts, time.Now().Add(ahead), e.live)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return r
}

func (e *stateEnv) live(box string) bool {
	return e.svc.registry.HasBox(alice, box)
}

func (e *stateEnv) restoreHead(t *testing.T) {
	t.Helper()
	p := statePath{user: alice, box: aliceBox}
	if _, err := e.svc.state.restore(e.ctx, p, machines.Endpoint{Key: "restore"}, headID(t, e.svc.state, p), noProgress); err != nil {
		t.Errorf("restore of the head: %v", err)
	}
}

func (e *stateEnv) headID(t *testing.T) string {
	t.Helper()
	h, err := e.svc.state.head(e.ctx, statePath{user: alice, box: aliceBox})
	if err != nil || h.Latest == "" {
		t.Fatalf("head = %v, %v", h, err)
	}
	return h.Latest
}

func (e *stateEnv) entry(t *testing.T, id string) timelineEntry {
	t.Helper()
	got, err := e.svc.state.keptEntry(e.ctx, statePath{user: alice, box: aliceBox}, id)
	if err != nil {
		t.Fatalf("entry %s: %v", id, err)
	}
	return got
}

func (e *stateEnv) finalEvent(t *testing.T, op *controlv1.Operation) *controlv1.OperationEvent {
	t.Helper()
	events := watch(t, e.svc, e.ctx, op.GetId(), "")
	return events[len(events)-1]
}

func (e *stateEnv) timelineIDs(t *testing.T, p statePath) []string {
	t.Helper()
	return timeline{Snapshots: e.timelineEntries(t, p)}.ids()
}

func (e *stateEnv) timelineEntries(t *testing.T, p statePath) []timelineEntry {
	t.Helper()
	var tl timeline
	if err := getJSON(e.ctx, e.store, p.timeline(), &tl); err != nil {
		t.Fatalf("timeline: %v", err)
	}
	return tl.Snapshots
}

func (e *stateEnv) indexIDs(t *testing.T) []string {
	t.Helper()
	var idx chunkIndex
	if err := getJSON(e.ctx, e.store, alice+"/index.json", &idx); err != nil {
		t.Fatalf("index: %v", err)
	}
	return idx.Chunks
}

func noAuth(ctx context.Context) context.Context { return ctx }

func saveLive(t *testing.T, l *stateLayer, p statePath, key string) {
	t.Helper()
	if _, err := l.save(t.Context(), noAuth, p, machines.Endpoint{Key: key}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress); err != nil {
		t.Fatalf("save of %s: %v", p.box, err)
	}
}

func newFamilyEnv(t *testing.T, image, defaultImage string, rebaseImages ...string) (*fakeMachines, *Service, context.Context) {
	t.Helper()
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Image: image, Revision: 1})
	st := newFakeStore()
	defaults := testDefaults
	defaults.Image, defaults.RebaseImages = defaultImage, rebaseImages
	svc := newService(fake, defaults, newFakeRegistry())
	svc.EnableState(newFakeAgent(st), st, &fakeKeys{})
	ctx := auth.WithUser(t.Context(), alice)
	if box, err := svc.get(ctx, aliceBox); err != nil || box.GetLatestImage() != image {
		t.Fatalf("box = %v, %v, want latest %s so no upgrade is offered", box, err, image)
	}
	return fake, svc, ctx
}

type clockSigner struct {
	fakeSigner

	mu  sync.Mutex
	now time.Time
}

func (c *clockSigner) Credential(string, string) string {
	return "exp:" + strconv.FormatInt(c.read().Add(credentialLifetime).Unix(), 10)
}

func (c *clockSigner) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clockSigner) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func credentialExpiry(t *testing.T, cred string) time.Time {
	t.Helper()
	secs, err := strconv.ParseInt(strings.TrimPrefix(cred, "exp:"), 10, 64)
	if err != nil {
		t.Fatalf("credential %q: %v", cred, err)
	}
	return time.Unix(secs, 0)
}

func hexID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

func chunkIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = hexID(fmt.Sprintf("c%d", i))
	}
	return ids
}

func everyBoxLive(string) bool { return true }

func snapshotID(n int) string {
	return fmt.Sprintf("%064x", n)
}

func headID(t *testing.T, l *stateLayer, p statePath) string {
	t.Helper()
	h, err := l.head(t.Context(), p)
	if err != nil || h.Latest == "" {
		t.Fatalf("head of %s = %+v, %v", p.box, h, err)
	}
	return h.Latest
}

func seed(t *testing.T, st *fakeStore, objects map[string]any) {
	t.Helper()
	for key, v := range objects {
		if raw, ok := v.(string); ok {
			st.objects[key] = []byte(raw)
			continue
		}
		if err := putJSON(t.Context(), st, key, v); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
}

type fakeStore struct {
	before func(op, key string)

	mu       sync.Mutex
	objects  map[string][]byte
	modified map[string]time.Time
	failures map[string]int
	puts     map[string]int
	deleted  []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, modified: map[string]time.Time{}, failures: map[string]int{}, puts: map[string]int{}}
}

func (s *fakeStore) PresignPut(_ context.Context, key string) (store.Presigned, error) {
	return store.Presigned{URL: "fake://put/" + key}, nil
}

func (s *fakeStore) PresignGet(_ context.Context, key string) (store.Presigned, error) {
	return store.Presigned{URL: "fake://get/" + key}, nil
}

func (s *fakeStore) Get(_ context.Context, key string) ([]byte, error) {
	s.hook("get", key)
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return data, nil
}

func (s *fakeStore) Put(_ context.Context, key string, data []byte) error {
	s.hook("put", key)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts[key]++
	if s.failures[key] > 0 {
		s.failures[key]--
		return fmt.Errorf("put %s: store unavailable", key)
	}
	s.objects[key], s.modified[key] = data, time.Now()
	return nil
}

func (s *fakeStore) List(_ context.Context, prefix string) ([]store.Object, error) {
	s.hook("list", prefix)
	s.mu.Lock()
	defer s.mu.Unlock()
	var objects []store.Object
	for key, data := range s.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, store.Object{Key: key, Size: int64(len(data)), Modified: s.modified[key]})
		}
	}
	return objects, nil
}

func (s *fakeStore) Delete(_ context.Context, keys []string) error {
	for _, key := range keys {
		s.hook("delete", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		delete(s.objects, key)
	}
	s.deleted = append(s.deleted, keys...)
	return nil
}

func (s *fakeStore) hook(op, key string) {
	if s.before != nil {
		s.before(op, key)
	}
}

func (s *fakeStore) failNext(key string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[key] = n
}

func (s *fakeStore) putCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts[key]
}

func (s *fakeStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

type versionedStore struct {
	*fakeStore
}

func newVersionedStore() *versionedStore {
	return &versionedStore{fakeStore: newFakeStore()}
}

func (s *versionedStore) GetVersion(ctx context.Context, key string) ([]byte, string, error) {
	data, err := s.Get(ctx, key)
	if err != nil {
		return nil, "", err
	}
	return data, hexID(string(data)), nil
}

func (s *versionedStore) PutIf(_ context.Context, key string, data []byte, version string) error {
	s.hook("putif", key)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.objects[key]
	if (version == "" && ok) || (version != "" && (!ok || hexID(string(cur)) != version)) {
		return store.ErrStale
	}
	s.puts[key]++
	s.objects[key], s.modified[key] = data, time.Now()
	return nil
}

func (s *versionedStore) layer(agent *fakeAgent) *stateLayer {
	l := newStateLayer(agent, s, &fakeKeys{})
	l.versioned = s
	return l
}

type fakeKeys struct {
	id string
}

func (k *fakeKeys) StateKey(_ context.Context, user string) (keyring.Key, error) {
	return keyring.Key{ID: cmp.Or(k.id, "test:"+user), Secret: make([]byte, keyring.KeySize)}, nil
}

type restoreCall struct {
	addr string
	reqs []*agentv1.RestoreRequest
}

func (c restoreCall) gets() []*agentv1.PresignedGet {
	var gets []*agentv1.PresignedGet
	for _, req := range c.reqs {
		gets = append(gets, req.GetGets()...)
	}
	return gets
}

type knownChunks struct {
	keyID  string
	chunks []string
}

type pinnedPlan struct {
	keyID    string
	chunks   []string
	spooled  map[string]bool
	manifest []byte
}

// fakeAgent plans snapshots over chunks (or byBox[ep.Key]) and moves objects in the fake store as presigned URLs name them.
type fakeAgent struct {
	store       *fakeStore
	chunks      []string
	byBox       map[string][]string
	files       []manifestFile
	changed     *uint64
	size        uint64
	snapshotID  string
	restoreErr  error
	restoreGate chan struct{}
	onUpload    func(ep machines.Endpoint)

	mu           sync.Mutex
	planReqs     []*agentv1.PlanRequest
	uploads      []*agentv1.UploadRequest
	restores     []restoreCall
	pathRestores []restoreCall
	snapshots    int
	known        map[string]knownChunks
	pinned       map[string]pinnedPlan
	planCred     string
	restoreCred  string
	uploadCreds  []string
}

func newFakeAgent(st *fakeStore) *fakeAgent {
	return &fakeAgent{store: st, known: map[string]knownChunks{}, pinned: map[string]pinnedPlan{}}
}

func (a *fakeAgent) Plan(ctx context.Context, _ string, ep machines.Endpoint, req *agentv1.PlanRequest, page dialer.PlanPageFunc) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.planCred = outgoingCredential(ctx)
	a.planReqs = append(a.planReqs, req)
	a.snapshots++
	resp := &agentv1.PlanResponse{SnapshotId: cmp.Or(a.snapshotID, snapshotID(a.snapshots)), Files: 7, LogicalBytes: 3 << 20, TakenAtMs: time.Now().UnixMilli(), ChangedFiles: a.changed}
	chunks := a.chunks
	if own, ok := a.byBox[ep.Key]; ok {
		chunks = own
	}
	known := a.known[ep.Key]
	plan := pinnedPlan{keyID: req.GetKey().GetKeyId(), chunks: chunks, spooled: map[string]bool{}}
	if a.files != nil {
		resp.SnapshotId, plan.manifest = sealManifest(req.GetKey().GetKey(), a.files)
		chunks = nil
		seen := map[string]bool{}
		for _, f := range a.files {
			for _, id := range f.Chunks {
				if !seen[id] {
					seen[id] = true
					chunks = append(chunks, id)
				}
			}
		}
		plan.chunks = chunks
	}
	for _, id := range chunks {
		resp.Chunks = append(resp.Chunks, &agentv1.ChunkRef{Id: id, Size: a.chunkSize()})
		if req.GetParent() == "" || known.keyID != plan.keyID || !slices.Contains(known.chunks, id) {
			plan.spooled[id] = true
		}
	}
	a.pinned[ep.Key] = plan
	pages := slices.Collect(slices.Chunk(resp.Chunks, 2))
	resp.Chunks = nil
	if len(pages) > 0 {
		resp.Chunks, pages = pages[0], pages[1:]
	}
	if err := page(resp); err != nil {
		return err
	}
	for _, chunks := range pages {
		if err := page(&agentv1.PlanResponse{Chunks: chunks}); err != nil {
			return err
		}
	}
	return nil
}

func (a *fakeAgent) Upload(ctx context.Context, _ string, ep machines.Endpoint, req *agentv1.UploadRequest, progress dialer.ProgressFunc) (*agentv1.TransferProgress, error) {
	a.mu.Lock()
	a.uploads = append(a.uploads, req)
	a.uploadCreds = append(a.uploadCreds, outgoingCredential(ctx))
	plan, planned := a.pinned[ep.Key]
	hook := a.onUpload
	a.mu.Unlock()
	for _, put := range req.GetPuts() {
		if planned && !plan.spooled[put.GetChunkId()] {
			return nil, status.Errorf(codes.InvalidArgument, "chunk %s was not spooled because the last upload held it; plan again without a parent", put.GetChunkId())
		}
	}
	if hook != nil {
		hook(ep)
	}
	puts := req.GetPuts()
	if req.GetManifest() != nil {
		puts = append(puts, req.GetManifest())
	}
	total := uint64(len(req.GetPuts())) * a.chunkSize()
	for i, put := range puts {
		body := []byte(put.GetChunkId())
		if put == req.GetManifest() && plan.manifest != nil {
			body = plan.manifest
		}
		if err := a.store.Put(ctx, strings.TrimPrefix(put.GetUrl(), "fake://put/"), body); err != nil {
			return nil, err
		}
		progress(&agentv1.TransferProgress{
			DoneBytes: min(uint64(i+1)*a.chunkSize(), total) / 2, TotalBytes: total / 2,
			DoneChunks: uint32(i + 1), TotalChunks: uint32(len(puts)),
		})
	}
	final := &agentv1.TransferProgress{DoneBytes: total / 2, TotalBytes: total / 2, DoneChunks: uint32(len(puts)), TotalChunks: uint32(len(puts)), Finished: req.GetManifest() != nil}
	if final.Finished {
		final.ManifestId = req.GetSnapshotId()
		a.mu.Lock()
		a.known[ep.Key] = knownChunks{keyID: plan.keyID, chunks: plan.chunks}
		a.mu.Unlock()
	}
	progress(final)
	return final, nil
}

func (a *fakeAgent) Restore(ctx context.Context, _ string, ep machines.Endpoint, reqs dialer.RestoreRequests, progress dialer.ProgressFunc) (*agentv1.TransferProgress, error) {
	return a.serveRestore(ctx, ep, reqs, progress, &a.restores)
}

func (a *fakeAgent) RestorePaths(ctx context.Context, _ string, ep machines.Endpoint, reqs dialer.RestoreRequests, progress dialer.ProgressFunc) (*agentv1.TransferProgress, error) {
	return a.serveRestore(ctx, ep, reqs, progress, &a.pathRestores)
}

func (a *fakeAgent) serveRestore(ctx context.Context, ep machines.Endpoint, reqs dialer.RestoreRequests, progress dialer.ProgressFunc, calls *[]restoreCall) (*agentv1.TransferProgress, error) {
	call := restoreCall{addr: ep.Key}
	for req, err := range reqs {
		if err != nil {
			return nil, err
		}
		call.reqs = append(call.reqs, req)
	}
	a.mu.Lock()
	a.restoreCred = outgoingCredential(ctx)
	*calls = append(*calls, call)
	a.mu.Unlock()
	if a.restoreGate != nil {
		<-a.restoreGate
	}
	if a.restoreErr != nil {
		return nil, a.restoreErr
	}
	manifest := call.reqs[0].GetManifest()
	for _, get := range append(call.gets(), manifest) {
		if _, err := a.store.Get(ctx, strings.TrimPrefix(get.GetUrl(), "fake://get/")); err != nil {
			return nil, fmt.Errorf("get %s: %w", get.GetUrl(), err)
		}
	}
	n := uint32(len(call.gets()))
	progress(&agentv1.TransferProgress{DoneBytes: 1, TotalBytes: 2, DoneChunks: n / 2, TotalChunks: n})
	final := &agentv1.TransferProgress{DoneBytes: 2, TotalBytes: 2, DoneChunks: n, TotalChunks: n, Finished: true, ManifestId: manifest.GetChunkId()}
	if scope := call.reqs[0].GetScope(); scope != nil {
		final.Target = scope.GetTarget() + "-2"
	}
	progress(final)
	return final, nil
}

func (a *fakeAgent) planCredential() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planCred
}

func (a *fakeAgent) uploadCredentials() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.uploadCreds)
}

func (a *fakeAgent) restoreCredential() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.restoreCred
}

func (a *fakeAgent) chunkSize() uint64 {
	return cmp.Or(a.size, 1<<20)
}

func (a *fakeAgent) lastPlan() *agentv1.PlanRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planReqs[len(a.planReqs)-1]
}

func (a *fakeAgent) allPlans() []*agentv1.PlanRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.planReqs)
}

func (a *fakeAgent) plans() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.planReqs)
}

func (a *fakeAgent) allUploads() []*agentv1.UploadRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.uploads)
}

func (a *fakeAgent) uploadCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.uploads)
}

func (a *fakeAgent) lastUpload() *agentv1.UploadRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.uploads[len(a.uploads)-1]
}

func (a *fakeAgent) restoreCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.restores)
}

func (a *fakeAgent) lastRestore() restoreCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.restores[len(a.restores)-1]
}

func (a *fakeAgent) pathRestoreCalls() []restoreCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.pathRestores)
}

type afterGetStore struct {
	*fakeStore
	after func(key string)
}

func (s *afterGetStore) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := s.fakeStore.Get(ctx, key)
	if s.after != nil {
		s.after(key)
	}
	return data, err
}

type presignedStore struct {
	*fakeStore
	presigner store.Store
}

func (s *presignedStore) PresignGet(ctx context.Context, key string) (store.Presigned, error) {
	return s.presigner.PresignGet(ctx, key)
}

type slowAgent struct {
	*fakeAgent
	skew    *atomic.Int64
	perPage time.Duration
	fetched int
}

func (a *slowAgent) Restore(ctx context.Context, _ string, _ machines.Endpoint, reqs dialer.RestoreRequests, progress dialer.ProgressFunc) (*agentv1.TransferProgress, error) {
	var urls []string
	for req, err := range reqs {
		if err != nil {
			return nil, err
		}
		if m := req.GetManifest(); m != nil {
			urls = append(urls, m.GetUrl())
		}
		for _, get := range req.GetGets() {
			urls = append(urls, get.GetUrl())
		}
		a.skew.Add(int64(a.perPage))
	}
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("get %s: %s", u, resp.Status)
		}
		a.fetched++
	}
	final := &agentv1.TransferProgress{Finished: true}
	progress(final)
	return final, nil
}

func expiringGets(skew *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		signed, err := time.Parse("20060102T150405Z", r.URL.Query().Get("X-Amz-Date"))
		if err != nil {
			http.Error(w, "AccessDenied", http.StatusForbidden)
			return
		}
		secs, err := strconv.Atoi(r.URL.Query().Get("X-Amz-Expires"))
		if err != nil {
			http.Error(w, "AccessDenied", http.StatusForbidden)
			return
		}
		if time.Now().Add(time.Duration(skew.Load())).After(signed.Add(time.Duration(secs) * time.Second)) {
			http.Error(w, "ExpiredToken", http.StatusBadRequest)
		}
	}
}

func savesOf(svc *Service, box string) []string {
	var ids []string
	for _, op := range svc.ops.all() {
		if op.boxID == box && op.phase == controlv1.Phase_PHASE_SAVING {
			ids = append(ids, op.id)
		}
	}
	return ids
}
