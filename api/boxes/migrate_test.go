package boxes

import (
	"slices"
	"testing"

	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

func TestMigrateStateCopiesOnceAndNeverMovesTheHeadBack(t *testing.T) {
	st := newFakeStore()
	old := aliceBox + "/"
	p := statePath{user: alice, box: aliceBox}
	seed(t, st, map[string]any{
		old + "chunks/" + hexID("c1"):                "sealed c1",
		old + "chunks/" + hexID("c2"):                "sealed c2",
		old + "manifests/" + snapshotID(1):           "sealed manifest",
		old + "snapshots/" + snapshotID(1) + ".json": snapshotRecord{ID: snapshotID(1), Chunks: []chunk{{ID: hexID("c1")}, {ID: hexID("c2")}}},
		old + "index.json":                           chunkIndex{KeyID: "file:k", Chunks: []string{hexID("c1"), hexID("c2")}},
		old + "head.json":                            head{Latest: snapshotID(1), SavedAtMs: 100},
	})
	if n, err := MigrateState(t.Context(), st, alice); err != nil || n != 4 {
		t.Fatalf("first migration copied %d, %v; want 4", n, err)
	}
	for _, key := range []string{p.chunk(hexID("c1")), p.chunk(hexID("c2")), p.manifest(snapshotID(1)), p.record(snapshotID(1))} {
		if string(st.objects[key]) == "" {
			t.Errorf("new layout lacks %s", key)
		}
	}
	assertHead(t, st, p, snapshotID(1))
	if n, err := MigrateState(t.Context(), st, alice); err != nil || n != 0 {
		t.Errorf("rerun copied %d, %v; want 0", n, err)
	}

	seed(t, st, map[string]any{
		old + "chunks/" + hexID("c3"):      "sealed c3",
		old + "manifests/" + snapshotID(2): "sealed manifest 2",
		old + "index.json":                 chunkIndex{KeyID: "file:k", Chunks: []string{hexID("c1"), hexID("c2"), hexID("c3")}},
		old + "head.json":                  head{Latest: snapshotID(2), SavedAtMs: 200},
	})
	if n, err := MigrateState(t.Context(), st, alice); err != nil || n != 2 {
		t.Errorf("migration after an old-layout save copied %d, %v; want 2", n, err)
	}
	assertHead(t, st, p, snapshotID(2))

	seed(t, st, map[string]any{
		p.index(): chunkIndex{KeyID: "file:k", Chunks: []string{hexID("c1"), hexID("c4")}},
		p.head():  head{Latest: snapshotID(3), SavedAtMs: 300},
	})
	if _, err := MigrateState(t.Context(), st, alice); err != nil {
		t.Fatalf("migration after a new-layout save: %v", err)
	}
	assertHead(t, st, p, snapshotID(3))
	var idx chunkIndex
	if err := getJSON(t.Context(), st, p.index(), &idx); err != nil || !slices.Equal(idx.Chunks, slices.Sorted(slices.Values([]string{hexID("c1"), hexID("c2"), hexID("c3"), hexID("c4")}))) {
		t.Errorf("merged index = %v, %v", idx.Chunks, err)
	}
}

func TestMigratedStateRestoresFromTheNewLayout(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1"), hexID("c2")}
	env.save(t)
	p := statePath{user: alice, box: aliceBox}
	old := aliceBox + "/"
	for from, to := range map[string]string{
		p.chunk(hexID("c1")):      old + "chunks/" + hexID("c1"),
		p.chunk(hexID("c2")):      old + "chunks/" + hexID("c2"),
		p.manifest(snapshotID(1)): old + "manifests/" + snapshotID(1),
		p.record(snapshotID(1)):   old + "snapshots/" + snapshotID(1) + ".json",
		p.index():                 old + "index.json",
		p.head():                  old + "head.json",
	} {
		env.store.objects[to] = env.store.objects[from]
		delete(env.store.objects, from)
	}
	env.svc.state.heads = map[statePath]head{}
	if _, err := MigrateState(env.ctx, env.store, alice); err != nil {
		t.Fatalf("MigrateState: %v", err)
	}
	if _, err := env.svc.state.restore(env.ctx, p, machines.Endpoint{Key: "new"}, "", func(*agentv1.TransferProgress) {}); err != nil {
		t.Fatalf("restore from the new layout: %v", err)
	}
	if gets := env.agent.lastRestore().gets(); len(gets) != 2 || gets[0].GetUrl() != "fake://get/"+p.chunk(hexID("c1")) {
		t.Errorf("restore gets = %v", gets)
	}
}

func TestMigrateStateRefusesWhatItDoesNotKnow(t *testing.T) {
	tests := []struct {
		name    string
		objects map[string]any
	}{
		{name: "unknown object", objects: map[string]any{aliceBox + "/tmp/x": "x"}},
		{name: "index under another key", objects: map[string]any{
			aliceBox + "/index.json": chunkIndex{KeyID: "file:k"},
			alice + "/index.json":    chunkIndex{KeyID: "users:k"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStore()
			seed(t, st, tt.objects)
			if _, err := MigrateState(t.Context(), st, alice); err == nil {
				t.Error("MigrateState succeeded")
			}
		})
	}
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

func assertHead(t *testing.T, st *fakeStore, p statePath, want string) {
	t.Helper()
	var h head
	if err := getJSON(t.Context(), st, p.head(), &h); err != nil || h.Latest != want {
		t.Errorf("head = %+v, %v; want %s", h, err, want)
	}
}
