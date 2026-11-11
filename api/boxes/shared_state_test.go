package boxes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

func TestSharedGCExcludesSaveCommitsUntilTimelineAndChunkDeletesFinish(t *testing.T) {
	for _, phase := range []string{"timeline", "chunks"} {
		t.Run(phase, func(t *testing.T) {
			st := newVersionedStore()
			agent := newFakeAgent(st.fakeStore)
			saver, collector := st.layer(agent), st.layer(agent)
			saver.shared = clustertest.Open(t)
			collector.shared = saver.shared
			p := statePath{user: alice, box: aliceBox}
			save := func(ctx context.Context) (*snapshotRecord, error) {
				return saver.save(ctx, noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress)
			}
			for _, chunks := range [][]string{{"c1", "c2"}, {"c2", "c3"}} {
				agent.byBox = map[string][]string{"a": hexIDs(chunks...)}
				if _, err := save(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			agent.byBox = map[string][]string{"a": hexIDs("c1", "c3")}
			ran := false
			st.before = func(op, key string) {
				if ran || !(phase == "timeline" && op == "putif" && key == p.timeline() || phase == "chunks" && op == "delete" && key == p.chunk(hexID("c1"))) {
					return
				}
				ran = true
				before, herr := saver.shared.Head(t.Context(), alice, aliceBox)
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				if _, err := save(ctx); err == nil {
					t.Errorf("save during %s committed", phase)
				}
				if after, err := saver.shared.Head(t.Context(), alice, aliceBox); herr != nil || err != nil || after.Version != before.Version {
					t.Errorf("head during %s = %+v, %v; want %+v unchanged", phase, after, err, before)
				}
			}
			if _, err := collector.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now().Add(40*24*time.Hour), everyBoxLive); err != nil {
				t.Fatal(err)
			}
			if !ran {
				t.Fatalf("gc did not reach %s", phase)
			}
			st.before = nil
			rec, err := save(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			tl, found, err := saver.readTimeline(t.Context(), p)
			if err != nil || !found || !slices.ContainsFunc(tl.Snapshots, func(e timelineEntry) bool { return e.ID == rec.ID }) {
				t.Fatalf("saved snapshot absent from timeline: %+v, %v", tl, err)
			}
			if !st.has(p.chunk(hexID("c1"))) {
				t.Fatal("gc deleted the chunk uploaded by the new save")
			}
			if _, err := saver.restore(t.Context(), p, machines.Endpoint{Key: "a"}, rec.ID, noProgress); err != nil {
				t.Fatalf("restore the saved snapshot: %v", err)
			}
		})
	}
}

func TestASharedSaveUploadsWhileTheStateLockIsHeldAndCommitsOnceItIsFree(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	saver := st.layer(agent)
	saver.shared = clustertest.Open(t)
	p := statePath{user: alice, box: aliceBox}
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	held, err := saver.shared.LockLong(t.Context(), "state:"+alice)
	if err != nil {
		t.Fatalf("hold the state lock: %v", err)
	}
	release := sync.OnceFunc(held)
	t.Cleanup(release)
	uploaded := make(chan struct{})
	agent.onUpload = func(machines.Endpoint) { close(uploaded) }
	done := make(chan error, 1)
	go func() {
		_, err := saver.save(t.Context(), noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress)
		done <- err
	}()
	select {
	case <-uploaded:
	case <-time.After(5 * time.Second):
		t.Fatal("the save did not upload while another holder had the state lock")
	}
	select {
	case err := <-done:
		t.Fatalf("the save committed under another holder's state lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("save after the lock was freed: %v", err)
	}
	if h, err := saver.head(t.Context(), p); err != nil || h.Latest == "" {
		t.Errorf("head after the save = %+v, %v; want the new snapshot", h, err)
	}
}

func TestASharedGCAndAnUploadingSaveOfOneProcessBothFinish(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	layer := st.layer(agent)
	layer.shared = clustertest.Open(t)
	p := statePath{user: alice, box: aliceBox}
	agent.byBox = map[string][]string{"a": hexIDs("c1", "c2")}
	saveLive(t, layer, p, "a")
	agent.byBox = map[string][]string{"a": hexIDs("c2", "c3")}
	collected := make(chan error, 1)
	agent.onUpload = func(machines.Endpoint) {
		agent.onUpload = nil
		go func() {
			_, err := layer.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now(), everyBoxLive)
			collected <- err
		}()
		time.Sleep(50 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := layer.save(ctx, noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress); err != nil {
		t.Fatalf("save beside a gc pass: %v", err)
	}
	select {
	case err := <-collected:
		if err != nil {
			t.Fatalf("gc pass beside a save: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the gc pass did not finish beside the save")
	}
}

func TestASaveWhoseUserStateAnotherReplicaDeletesDuringItsUploadLeavesNothing(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	layer := st.layer(agent)
	shared, dsn := clustertest.OpenDSN(t)
	layer.shared = shared
	other := st.layer(agent)
	other.shared = clustertest.OpenReplica(t, dsn)
	svc := &Service{state: other}
	p := statePath{user: alice, box: aliceBox}
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	saveLive(t, layer, p, "a")
	agent.byBox = map[string][]string{"a": hexIDs("c1", "c2")}
	deleted := make(chan error, 1)
	agent.onUpload = func(machines.Endpoint) {
		agent.onUpload = nil
		deleted <- svc.DeleteState(t.Context(), alice)
	}
	_, err := layer.save(t.Context(), noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress)
	if err := <-deleted; err != nil {
		t.Fatalf("DeleteState during the upload: %v", err)
	}
	if !errors.Is(err, errStateDeleted) {
		t.Errorf("save across DeleteState = %v; want errStateDeleted", err)
	}
	if left, err := st.List(t.Context(), alice+"/"); err != nil || len(left) > 0 {
		t.Errorf("objects under the deleted user = %v, %v; want none", left, err)
	}
}

func TestAFirstSaveWhoseUserStateAnotherReplicaDeletesDuringItsUploadLeavesNothing(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	layer := st.layer(agent)
	shared, dsn := clustertest.OpenDSN(t)
	layer.shared = shared
	other := st.layer(agent)
	other.shared = clustertest.OpenReplica(t, dsn)
	svc := &Service{state: other}
	p := statePath{user: alice, box: aliceBox}
	agent.byBox = map[string][]string{"a": hexIDs("c1", "c2")}
	deleted := make(chan error, 1)
	agent.onUpload = func(machines.Endpoint) {
		agent.onUpload = nil
		deleted <- svc.DeleteState(t.Context(), alice)
	}
	_, err := layer.save(t.Context(), noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress)
	if err := <-deleted; err != nil {
		t.Fatalf("DeleteState during the upload: %v", err)
	}
	if !errors.Is(err, errStateDeleted) {
		t.Errorf("first save across DeleteState = %v; want errStateDeleted", err)
	}
	if left, err := st.List(t.Context(), alice+"/"); err != nil || len(left) > 0 {
		t.Errorf("objects under the deleted user = %v, %v; want none", left, err)
	}
}

func TestAFirstSaveAfterAnotherReplicaDeletedTheAccountWritesNoIndex(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	layer := st.layer(agent)
	shared, dsn := clustertest.OpenDSN(t)
	layer.shared = shared
	other := st.layer(agent)
	other.shared = clustertest.OpenReplica(t, dsn)
	svc := &Service{state: other}
	var gone atomic.Bool
	layer.accountExists = func(context.Context, string) (bool, error) { return !gone.Load(), nil }
	p := statePath{user: alice, box: aliceBox}
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	var once sync.Once
	st.before = func(action, key string) {
		if action != "get" || key != p.index() {
			return
		}
		once.Do(func() {
			if err := svc.DeleteState(t.Context(), alice); err != nil {
				t.Errorf("DeleteState before the save reads the index: %v", err)
			}
			gone.Store(true)
		})
	}
	if _, err := layer.save(t.Context(), noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress); !errors.Is(err, errStateDeleted) {
		t.Errorf("first save of a deleted account = %v; want errStateDeleted", err)
	}
	if left, err := st.List(t.Context(), alice+"/"); err != nil || len(left) > 0 {
		t.Errorf("objects under the deleted user = %v, %v; want none", left, err)
	}
}

func TestARestoreWritesNoManifestAfterAnotherReplicaDeletesTheUsersState(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	layer := st.layer(agent)
	shared, dsn := clustertest.OpenDSN(t)
	layer.shared = shared
	layer.deltaFor = func(string) bool { return true }
	other := st.layer(agent)
	other.shared = clustertest.OpenReplica(t, dsn)
	svc := &Service{state: other}
	p := statePath{user: alice, box: aliceBox}
	second := compactedSecond(t, st, agent, layer, p)
	var once sync.Once
	st.before = func(action, key string) {
		if action != "put" || key != p.manifest(second) {
			return
		}
		once.Do(func() { deleteDuring(t, svc, clustertest.AdvisoryWaiting(t, dsn)) })
	}
	_, _ = layer.restore(t.Context(), p, machines.Endpoint{Key: "a"}, second, noProgress)
	clustertest.WaitFor(t, "no object of the deleted user", func() bool {
		left, err := st.List(t.Context(), alice+"/")
		return err == nil && len(left) == 0
	})
}

func TestARestoreDoesNotWaitForAGCPassOfTheUsersState(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	layer := st.layer(agent)
	layer.shared = clustertest.Open(t)
	layer.deltaFor = func(string) bool { return true }
	p := statePath{user: alice, box: aliceBox}
	second := compactedSecond(t, st, agent, layer, p)
	collecting, err := layer.shared.LockLong(t.Context(), "state:"+alice)
	if err != nil {
		t.Fatalf("hold the state lock: %v", err)
	}
	t.Cleanup(collecting)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := layer.restore(ctx, p, machines.Endpoint{Key: "a"}, second, noProgress); err != nil {
		t.Fatalf("restore while a GC pass holds the user's state lock: %v", err)
	}
	if !st.has(p.manifest(second)) {
		t.Error("the restore wrote no manifest of the compacted snapshot")
	}
}

func TestADeleteOfTheUsersStateWaitsForARestoreOfAnotherReplica(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	layer := st.layer(agent)
	shared, dsn := clustertest.OpenDSN(t)
	layer.shared = shared
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	saveLive(t, layer, statePath{user: alice, box: aliceBox}, "a")
	restoring, err := clustertest.OpenReplica(t, dsn).LockLongAll(t.Context(), cluster.Advisory{Name: "delete:" + alice, Shared: true})
	if err != nil {
		t.Fatalf("hold the delete lock shared: %v", err)
	}
	release := sync.OnceFunc(restoring)
	t.Cleanup(release)
	deleted := make(chan error, 1)
	go func() { deleted <- (&Service{state: layer}).DeleteState(t.Context(), alice) }()
	clustertest.WaitFor(t, "the delete waits for the restore", clustertest.AdvisoryWaiting(t, dsn))
	if left, err := st.List(t.Context(), alice+"/"); err != nil || len(left) == 0 {
		t.Errorf("objects of the user while the restore holds the delete lock = %v, %v; want them kept", left, err)
	}
	release()
	if err := <-deleted; err != nil {
		t.Fatalf("DeleteState: %v", err)
	}
	if left, err := st.List(t.Context(), alice+"/"); err != nil || len(left) > 0 {
		t.Errorf("objects of the deleted user = %v, %v; want none", left, err)
	}
}

func TestAWakeRestoreReadsItsDeltaWhileAnotherReplicaPrunesItsBase(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	saver, restorer, collector := st.layer(agent), st.layer(agent), st.layer(agent)
	shared, dsn := clustertest.OpenDSN(t)
	saver.shared, restorer.shared = shared, shared
	collector.shared = clustertest.OpenReplica(t, dsn)
	saver.deltaFor = func(string) bool { return true }
	p := statePath{user: alice, box: aliceBox}
	live := func(string) bool { return true }
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	agent.files = manyFiles(64, 0)
	saveLive(t, saver, p, "a")
	if _, err := saver.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now(), live); err != nil {
		t.Fatalf("collect: %v", err)
	}
	agent.files = manyFiles(64, 1)
	saveLive(t, saver, p, "a")
	h, err := saver.head(t.Context(), p)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	var file recordFile
	if err := getJSON(t.Context(), st, p.record(h.Latest), &file); err != nil || file.Base == "" {
		t.Fatalf("head record base %q, %v; want a delta", file.Base, err)
	}
	base := file.Base
	pruned := make(chan error, 1)
	waiting := clustertest.AdvisoryWaiting(t, dsn)
	var hooked atomic.Bool
	st.before = func(action, key string) {
		if action != "get" || key != p.record(base) || !hooked.CompareAndSwap(false, true) {
			return
		}
		go func() {
			_, err := collector.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now().Add(40*24*time.Hour), live)
			pruned <- err
		}()
		clustertest.WaitFor(t, "the prune ends or waits on the box lock", func() bool { return len(pruned) > 0 || waiting() })
	}
	if _, err := restorer.restore(t.Context(), p, machines.Endpoint{Key: "a"}, h.Latest, noProgress); err != nil {
		t.Errorf("restore while another replica prunes the head's base: %v", err)
	}
	if err := <-pruned; err != nil {
		t.Errorf("collect: %v", err)
	}
}

func TestARestoreOfPathsReadsItsDeltaWhileAnotherReplicaPrunesItsBase(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	saver, restorer, collector := st.layer(agent), st.layer(agent), st.layer(agent)
	shared, dsn := clustertest.OpenDSN(t)
	saver.shared, restorer.shared = shared, shared
	collector.shared = clustertest.OpenReplica(t, dsn)
	saver.deltaFor = func(string) bool { return true }
	p := statePath{user: alice, box: aliceBox}
	live := func(string) bool { return true }
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	agent.files = manyFiles(64, 0)
	saveLive(t, saver, p, "a")
	if _, err := saver.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now(), live); err != nil {
		t.Fatalf("collect: %v", err)
	}
	agent.files = manyFiles(64, 1)
	saveLive(t, saver, p, "a")
	h, err := saver.head(t.Context(), p)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	var file recordFile
	if err := getJSON(t.Context(), st, p.record(h.Latest), &file); err != nil || file.Base == "" {
		t.Fatalf("head record base %q, %v; want a delta", file.Base, err)
	}
	e, err := restorer.keptEntry(t.Context(), p, h.Latest)
	if err != nil {
		t.Fatalf("keptEntry: %v", err)
	}
	bp, err := parseBoxPath("/workspace/src/f-0001.go")
	if err != nil {
		t.Fatalf("parseBoxPath: %v", err)
	}
	unpin, err := restorer.pin(t.Context(), p, e.ID)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	defer unpin()
	pruned := make(chan error, 1)
	waiting := clustertest.AdvisoryWaiting(t, dsn)
	var hooked atomic.Bool
	st.before = func(action, key string) {
		if action != "get" || key != p.record(file.Base) || !hooked.CompareAndSwap(false, true) {
			return
		}
		go func() {
			_, err := collector.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now().Add(40*24*time.Hour), live)
			pruned <- err
		}()
		clustertest.WaitFor(t, "the prune ends or waits on the box lock", func() bool { return len(pruned) > 0 || waiting() })
	}
	if _, err := restorer.restorePaths(t.Context(), p, machines.Endpoint{Key: "a"}, e, []boxPath{bp}, noProgress); err != nil {
		t.Errorf("restorePaths while another replica prunes the snapshot's base: %v", err)
	}
	if err := <-pruned; err != nil {
		t.Errorf("collect: %v", err)
	}
}

func TestARestoreOfPathsKeepsTheManifestAnotherReplicaCompacts(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	layer, collector := st.layer(agent), st.layer(agent)
	shared, dsn := clustertest.OpenDSN(t)
	layer.shared = shared
	collector.shared = clustertest.OpenReplica(t, dsn)
	layer.deltaFor = func(string) bool { return true }
	p := statePath{user: alice, box: aliceBox}
	live := func(string) bool { return true }
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	agent.files = manyFiles(64, 0)
	saveLive(t, layer, p, "a")
	if _, err := layer.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now(), live); err != nil {
		t.Fatalf("collect: %v", err)
	}
	agent.files = manyFiles(64, 1)
	saveLive(t, layer, p, "a")
	h, err := layer.head(t.Context(), p)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	second := h.Latest
	agent.files = manyFiles(64, 2)
	saveLive(t, layer, p, "a")
	e, err := layer.keptEntry(t.Context(), p, second)
	if err != nil {
		t.Fatalf("keptEntry: %v", err)
	}
	bp, err := parseBoxPath("/workspace/src/f-0001.go")
	if err != nil {
		t.Fatalf("parseBoxPath: %v", err)
	}
	restored := make(chan error, 1)
	waiting := clustertest.AdvisoryWaiting(t, dsn)
	var hooked atomic.Bool
	st.before = func(action, key string) {
		if action != "get" || key != p.manifest(second) || !hooked.CompareAndSwap(false, true) {
			return
		}
		go func() {
			unpin, err := layer.pin(t.Context(), p, second)
			if err == nil {
				_, err = layer.restorePaths(t.Context(), p, machines.Endpoint{Key: "a"}, e, []boxPath{bp}, noProgress)
				unpin()
			}
			restored <- err
		}()
		clustertest.WaitFor(t, "the restore ends or waits on the box lock", func() bool { return len(restored) > 0 || waiting() })
	}
	if _, err := collector.collect(t.Context(), alice, GCOptions{DeleteBatch: 1000}, time.Now(), live); err != nil {
		t.Errorf("collect: %v", err)
	}
	if err := <-restored; err != nil {
		t.Errorf("restorePaths: %v", err)
	}
	if !st.has(p.manifest(second)) {
		t.Error("the compaction removed the full manifest the restore materialized")
	}
}

func TestSavesWaitingForTheirCommitKeepTheirSaveSlots(t *testing.T) {
	st := newVersionedStore()
	agent := newFakeAgent(st.fakeStore)
	layer := st.layer(agent)
	layer.shared = clustertest.Open(t)
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	held, err := layer.shared.LockLong(t.Context(), "state:"+alice)
	if err != nil {
		t.Fatalf("hold the state lock: %v", err)
	}
	release := sync.OnceFunc(held)
	t.Cleanup(release)
	planned := func() int {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return len(agent.planReqs)
	}
	done := make(chan error, saveFanout+1)
	for i := range saveFanout + 1 {
		p := statePath{user: alice, box: fmt.Sprintf("cb-commit-%d", i)}
		go func() {
			_, err := layer.save(t.Context(), noAuth, p, machines.Endpoint{Key: "a"}, "", false, agentv1.Consistency_CONSISTENCY_LIVE, noProgress)
			done <- err
		}()
	}
	clustertest.WaitFor(t, "saveFanout plans", func() bool { return planned() == saveFanout })
	time.Sleep(200 * time.Millisecond)
	if got := planned(); got != saveFanout {
		t.Errorf("plans while every commit waits = %d, want %d of %d", got, saveFanout, saveFanout+1)
	}
	release()
	for range saveFanout + 1 {
		if err := <-done; err != nil {
			t.Errorf("save: %v", err)
		}
	}
}

func compactedSecond(t *testing.T, st *versionedStore, agent *fakeAgent, layer *stateLayer, p statePath) string {
	t.Helper()
	live := func(string) bool { return true }
	agent.byBox = map[string][]string{"a": hexIDs("c1")}
	agent.files = manyFiles(64, 0)
	saveLive(t, layer, p, "a")
	if _, err := layer.collect(t.Context(), p.user, GCOptions{DeleteBatch: 1000}, time.Now(), live); err != nil {
		t.Fatalf("collect: %v", err)
	}
	agent.files = manyFiles(64, 1)
	saveLive(t, layer, p, "a")
	h, err := layer.head(t.Context(), p)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	agent.files = manyFiles(64, 2)
	saveLive(t, layer, p, "a")
	if _, err := layer.collect(t.Context(), p.user, GCOptions{DeleteBatch: 1000}, time.Now(), live); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if st.has(p.manifest(h.Latest)) {
		t.Fatal("the second snapshot's manifest was not compacted")
	}
	return h.Latest
}
