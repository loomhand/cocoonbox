package boxes

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

func TestSharedGCExcludesSavesUntilTimelineAndChunkDeletesFinish(t *testing.T) {
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
				plans := agent.plans()
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				if _, err := save(ctx); err == nil || agent.plans() != plans {
					t.Errorf("save during %s: %v; plans %d -> %d", phase, err, plans, agent.plans())
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
