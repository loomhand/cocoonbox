package boxes

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

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
