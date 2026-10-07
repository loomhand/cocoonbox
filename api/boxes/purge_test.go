package boxes

import (
	"testing"
)

func TestDeleteStateRemovesTheUsersObjectsAndNoOthers(t *testing.T) {
	env := newStateEnv(t)
	neighbour := alice + "0"
	for _, user := range []string{alice, neighbour, "bob"} {
		p := statePath{user: user, box: BoxID(user)}
		seed(t, env.store, map[string]any{
			p.head():                  head{Latest: snapshotID(1)},
			p.index():                 chunkIndex{KeyID: "file:k", Chunks: []string{hexID("c1")}},
			p.chunk(hexID("c1")):      "sealed c1",
			p.manifest(snapshotID(1)): "sealed manifest",
			p.record(snapshotID(1)):   snapshotRecord{ID: snapshotID(1)},
			p.timeline():              timeline{},
		})
	}
	if left, _ := env.store.List(t.Context(), alice+"/"); len(left) != 6 {
		t.Fatalf("seeded %d objects for the user, want 6", len(left))
	}
	if err := env.svc.DeleteState(t.Context(), alice); err != nil {
		t.Fatalf("DeleteState: %v", err)
	}
	if left, _ := env.store.List(t.Context(), alice+"/"); len(left) != 0 {
		t.Errorf("%d objects of the user remain: %v", len(left), left)
	}
	for _, user := range []string{neighbour, "bob"} {
		if left, _ := env.store.List(t.Context(), user+"/"); len(left) != 6 {
			t.Errorf("%s keeps %d objects, want 6", user, len(left))
		}
	}
	if err := env.svc.DeleteState(t.Context(), alice); err != nil {
		t.Errorf("DeleteState again: %v", err)
	}
	if n, err := DeleteState(t.Context(), env.store, "nobody"); err != nil || n != 0 {
		t.Errorf("DeleteState of an absent prefix = %d, %v; want 0, nil", n, err)
	}
}
