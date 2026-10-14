package boxes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

func TestDeltaRecordsHoldOnlyTheChangeAndReadBackFull(t *testing.T) {
	env := newDeltaEnv(t)
	p := statePath{user: alice, box: aliceBox}
	env.agent.chunks = chunkIDs(400)
	env.save(t)
	env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
	env.agent.chunks = append(chunkIDs(399), hexID("new"))
	env.save(t)
	second := env.headID(t)

	var file recordFile
	if err := getJSON(env.ctx, env.store, p.record(second), &file); err != nil {
		t.Fatalf("record: %v", err)
	}
	if file.Base != snapshotID(1) || !bytes.HasPrefix(file.Chunks, []byte("{")) {
		t.Fatalf("second record base %q chunks %s, want a delta against the first", file.Base, file.Chunks)
	}
	if full, delta := len(env.store.objects[p.record(snapshotID(1))]), len(env.store.objects[p.record(second)]); delta*10 > full {
		t.Errorf("delta record is %d bytes against %d full", delta, full)
	}
	var old struct {
		timelineEntry
		Chunks []chunk `json:"chunks"`
	}
	if err := json.Unmarshal(env.store.objects[p.record(second)], &old); err == nil {
		t.Error("an api without delta records decodes a delta record as a chunk list")
	}
	rec, err := env.svc.state.record(env.ctx, p, second)
	if err != nil {
		t.Fatalf("read delta record: %v", err)
	}
	if got := chunkSet(rec.Chunks); !maps.Equal(got, idSet(env.agent.chunks)) {
		t.Errorf("rebuilt chunks = %d ids, want the planned %d", len(got), len(env.agent.chunks))
	}
	env.restoreHead(t)
	if got := idSet(getIDs(env.agent.lastRestore())); !maps.Equal(got, idSet(env.agent.chunks)) {
		t.Errorf("restore presigned %d chunks, want the head's %d", len(got), len(env.agent.chunks))
	}
	if tl := env.timelineEntries(t, p); tl[len(tl)-1].Base != snapshotID(1) {
		t.Errorf("timeline entry of the delta has base %q", tl[len(tl)-1].Base)
	}

	env.agent.chunks = chunkIDs(200)
	env.save(t)
	var third recordFile
	if err := getJSON(env.ctx, env.store, p.record(env.headID(t)), &third); err != nil || third.Base != "" {
		t.Errorf("a change of half the base was saved as a delta (base %q, %v)", third.Base, err)
	}
	if base, err := env.svc.state.deltaBase(env.ctx, p, time.Now().Add(24*time.Hour)); err != nil || base != nil {
		t.Errorf("tomorrow's saves would use base %v, %v; want a new full snapshot", base, err)
	}
}

func TestGCCompactsDeltaManifestsAndRebuildsThemExactly(t *testing.T) {
	env := newDeltaEnv(t)
	p := statePath{user: alice, box: aliceBox}
	env.agent.files = manyFiles(64, 0)
	env.save(t)
	env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
	env.agent.files = manyFiles(64, 1)
	env.save(t)
	second := env.headID(t)
	env.agent.files = manyFiles(64, 2)
	env.save(t)
	head := env.headID(t)
	fullSize := len(env.store.objects[p.manifest(second)])

	r := env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
	if r.compacted != 1 || env.store.has(p.manifest(second)) || !env.store.has(p.manifest(second)+deltaSuffix) || !env.store.has(p.manifest(head)) {
		t.Fatalf("compacted %d; full %v, delta %v, head full %v", r.compacted, env.store.has(p.manifest(second)), env.store.has(p.manifest(second)+deltaSuffix), env.store.has(p.manifest(head)))
	}
	if deltaSize := len(env.store.objects[p.manifest(second)+deltaSuffix]); deltaSize*3 > fullSize {
		t.Errorf("manifest delta is %d bytes against %d full", deltaSize, fullSize)
	}
	key, _ := env.keys.StateKey(env.ctx, alice)
	plain, err := env.svc.state.manifestPlain(env.ctx, p, key, second)
	if err != nil || !matchesID(key.Secret, second, plain) {
		t.Fatalf("rebuilt manifest: %v", err)
	}
	resp, err := env.svc.BrowseSnapshot(env.ctx, &controlv1.BrowseSnapshotRequest{BoxId: aliceBox, SnapshotId: second, Dir: "/workspace/src"})
	if err != nil || len(resp.GetEntries()) != 64 {
		t.Fatalf("browse of a compacted snapshot: %d entries, %v", len(resp.GetEntries()), err)
	}

	op, err := env.svc.RestorePaths(env.ctx, &controlv1.RestorePathsRequest{BoxId: aliceBox, SnapshotId: second, Paths: []string{"/workspace/src/f-0001.go"}})
	if err != nil {
		t.Fatalf("RestorePaths: %v", err)
	}
	if last := env.finalEvent(t, op); last.GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("restore of a compacted snapshot ended with %v", last)
	}
	if !env.store.has(p.manifest(second)) {
		t.Error("the restore gave the agent no full manifest")
	}
	if r := env.collect(t, GCOptions{DeleteBatch: 1000}, 0); r.compacted != 1 || env.store.has(p.manifest(second)) {
		t.Errorf("the next pass compacted %d and left the full manifest %v", r.compacted, env.store.has(p.manifest(second)))
	}
}

func TestPruningMakesTheOnlyDependentOfADroppedBaseFull(t *testing.T) {
	env := newDeltaEnv(t)
	p := statePath{user: alice, box: aliceBox}
	env.agent.files = manyFiles(64, 0)
	env.save(t)
	base := env.headID(t)
	env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
	env.agent.files = manyFiles(64, 1)
	env.save(t)
	head := env.headID(t)

	r := env.collect(t, GCOptions{DeleteBatch: 1000}, 40*24*time.Hour)
	if r.expanded != 1 || env.store.has(p.record(base)) || env.store.has(p.manifest(base)) {
		t.Fatalf("expanded %d; base record kept %v, base manifest kept %v", r.expanded, env.store.has(p.record(base)), env.store.has(p.manifest(base)))
	}
	var file recordFile
	if err := getJSON(env.ctx, env.store, p.record(head), &file); err != nil || file.Base != "" || !bytes.HasPrefix(file.Chunks, []byte("[")) {
		t.Fatalf("head record base %q, %v", file.Base, err)
	}
	if tl := env.timelineEntries(t, p); len(tl) != 1 || tl[0].ID != head || tl[0].Base != "" {
		t.Errorf("timeline = %+v", tl)
	}
	env.restoreHead(t)
	key, _ := env.keys.StateKey(env.ctx, alice)
	if _, err := env.svc.state.manifestPlain(env.ctx, p, key, head); err != nil {
		t.Errorf("head manifest without its base: %v", err)
	}
}

func TestPruningKeepsABaseThatSeveralSnapshotsNeed(t *testing.T) {
	env := newDeltaEnv(t)
	p := statePath{user: alice, box: aliceBox}
	env.agent.files = manyFiles(64, 0)
	env.save(t)
	base := env.headID(t)
	env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
	env.agent.files = manyFiles(64, 1)
	env.save(t)
	second := env.headID(t)
	env.agent.files = manyFiles(64, 2)
	env.save(t)
	head := env.headID(t)
	env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
	unpin := env.svc.state.pin(p, second)
	defer unpin()

	r := env.collect(t, GCOptions{DeleteBatch: 1000}, 40*24*time.Hour)
	if r.expanded != 0 || !env.store.has(p.record(base)) || !env.store.has(p.manifest(base)) {
		t.Fatalf("expanded %d; base record %v, base manifest %v", r.expanded, env.store.has(p.record(base)), env.store.has(p.manifest(base)))
	}
	if ids := env.timelineIDs(t, p); !slices.Equal(ids, []string{second, head}) {
		t.Errorf("timeline = %v, want the pinned snapshot and the head", ids)
	}
	key, _ := env.keys.StateKey(env.ctx, alice)
	for _, id := range []string{second, head} {
		rec, err := env.svc.state.record(env.ctx, p, id)
		if err != nil || rec.Base != base {
			t.Fatalf("record %s: base %q, %v", id, rec.Base, err)
		}
		for _, c := range rec.Chunks {
			if !env.store.has(p.chunk(c.ID)) {
				t.Errorf("the sweep deleted chunk %s of %s", c.ID, id)
			}
		}
		if _, err := env.svc.state.manifestPlain(env.ctx, p, key, id); err != nil {
			t.Errorf("manifest %s: %v", id, err)
		}
	}
}

func TestGCAndADeltaSaveInterleave(t *testing.T) {
	for _, gcFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("gc first %v", gcFirst), func(t *testing.T) {
			env := newDeltaEnv(t)
			p := statePath{user: alice, box: aliceBox}
			env.agent.files = manyFiles(64, 0)
			env.save(t)
			env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
			env.agent.files = manyFiles(64, 1)
			env.save(t)
			env.agent.files = manyFiles(64, 2)

			hold, held := make(chan struct{}), make(chan struct{})
			var once sync.Once
			if gcFirst {
				env.store.before = func(op, key string) {
					if op == "put" && key == p.timeline() {
						once.Do(func() { close(held); <-hold })
					}
				}
			} else {
				env.agent.onUpload = func(machines.Endpoint) { once.Do(func() { close(held); <-hold }) }
			}
			done := make(chan error, 2)
			collect := func() {
				_, err := env.svc.state.collect(env.ctx, alice, GCOptions{DeleteBatch: 1000}, time.Now().Add(40*24*time.Hour), env.live)
				done <- err
			}
			save := func() { done <- env.trySave() }
			first, second := save, collect
			if gcFirst {
				first, second = collect, save
			}
			go first()
			<-held
			go second()
			time.Sleep(50 * time.Millisecond)
			close(hold)
			for range 2 {
				if err := <-done; err != nil {
					t.Fatalf("collect or save: %v", err)
				}
			}
			env.store.before = nil
			head := env.headID(t)
			rec, err := env.svc.state.record(env.ctx, p, head)
			if err != nil {
				t.Fatalf("head record: %v", err)
			}
			for _, c := range rec.Chunks {
				if !env.store.has(p.chunk(c.ID)) {
					t.Errorf("chunk %s of the head is gone", c.ID)
				}
			}
			key, _ := env.keys.StateKey(env.ctx, alice)
			if _, err := env.svc.state.manifestPlain(env.ctx, p, key, head); err != nil {
				t.Errorf("head manifest: %v", err)
			}
			if rec.Base != "" && (!env.store.has(p.record(rec.Base)) || !env.store.has(p.manifest(rec.Base))) {
				t.Errorf("the head's base %s is gone", rec.Base)
			}
			env.restoreHead(t)
		})
	}
}

func TestExpandWritesTheLayoutOfAnAPIWithoutDeltas(t *testing.T) {
	env := newDeltaEnv(t)
	p := statePath{user: alice, box: aliceBox}
	for i := range 4 {
		env.agent.files = manyFiles(64, i)
		env.save(t)
		if i == 0 {
			env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
		}
	}
	env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
	before := map[string]map[string]bool{}
	for _, id := range env.timelineIDs(t, p) {
		rec, err := env.svc.state.record(env.ctx, p, id)
		if err != nil {
			t.Fatalf("record %s: %v", id, err)
		}
		before[id] = chunkSet(rec.Chunks)
	}

	n, err := ExpandState(env.ctx, env.store, env.keys, alice)
	if err != nil || n != 3 {
		t.Fatalf("ExpandState = %d, %v; want the three deltas", n, err)
	}
	key, _ := env.keys.StateKey(env.ctx, alice)
	for id, chunks := range before {
		var old struct {
			timelineEntry
			Chunks []chunk `json:"chunks"`
		}
		if err := json.Unmarshal(env.store.objects[p.record(id)], &old); err != nil || old.Base != "" || !maps.Equal(chunkSet(old.Chunks), chunks) {
			t.Errorf("record %s for an old api: base %q, %v", id, old.Base, err)
		}
		if _, err := openSealed(key.Secret, id, env.store.objects[p.manifest(id)], manifestLimit); err != nil {
			t.Errorf("manifest %s for an old agent: %v", id, err)
		}
		if env.store.has(p.manifest(id) + deltaSuffix) {
			t.Errorf("manifest delta of %s survived", id)
		}
	}
	for _, e := range env.timelineEntries(t, p) {
		if e.Base != "" {
			t.Errorf("timeline entry %s keeps base %s", e.ID, e.Base)
		}
	}
}

func TestMetadataBytesPerSave(t *testing.T) {
	if os.Getenv("COCOONBOX_MEASURE") == "" {
		t.Skip("COCOONBOX_MEASURE is unset")
	}
	const files, changed = 100_000, 40
	for _, deltas := range []bool{false, true} {
		env := newStateEnv(t)
		if deltas {
			env.svc.EnableDeltas([]string{alice})
		}
		p := statePath{user: alice, box: aliceBox}
		env.agent.files = manyFiles(files, 0)
		if err := env.trySave(); err != nil {
			t.Fatalf("save: %v", err)
		}
		env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
		for i := 1; i <= 2; i++ {
			env.agent.files = manyFiles(files, i)
			for j := range changed {
				env.agent.files[j*997].MtimeMs += int64(i)
			}
			start := time.Now()
			if err := env.trySave(); err != nil {
				t.Fatalf("save: %v", err)
			}
			took := time.Since(start)
			id := env.headID(t)
			sizes := map[string]int{
				"record": len(env.store.objects[p.record(id)]), "manifest": len(env.store.objects[p.manifest(id)]),
				"index": len(env.store.objects[p.index()]), "timeline": len(env.store.objects[p.timeline()]), "head": len(env.store.objects[p.head()]),
			}
			t.Logf("deltas %v, save %d: written record %d, manifest %d, index %d, timeline %d, head %d B; save %s", deltas, i, sizes["record"], sizes["manifest"], sizes["index"], sizes["timeline"], sizes["head"], took.Round(time.Millisecond))
		}
		first := env.headID(t)
		env.agent.files = manyFiles(files, 3)
		if err := env.trySave(); err != nil {
			t.Fatalf("save: %v", err)
		}
		start := time.Now()
		env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
		t.Logf("deltas %v: after a GC pass (%s) the snapshot before the head keeps record %d B and manifest %d B (full) + %d B (delta)", deltas, time.Since(start).Round(time.Millisecond),
			len(env.store.objects[p.record(first)]), len(env.store.objects[p.manifest(first)]), len(env.store.objects[p.manifest(first)+deltaSuffix]))
	}
}

func newDeltaEnv(t *testing.T) *stateEnv {
	t.Helper()
	env := newStateEnv(t)
	env.svc.EnableDeltas([]string{alice})
	return env
}

func (e *stateEnv) trySave() error {
	op, err := e.svc.SaveState(e.ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		return err
	}
	for {
		events, done, changed := e.svc.ops.get(op.GetId()).since(0)
		if done {
			if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_DONE {
				return fmt.Errorf("save ended with %v", last)
			}
			return nil
		}
		<-changed
	}
}

func (e *stateEnv) timelineEntries(t *testing.T, p statePath) []timelineEntry {
	t.Helper()
	var tl timeline
	if err := getJSON(e.ctx, e.store, p.timeline(), &tl); err != nil {
		t.Fatalf("timeline: %v", err)
	}
	return tl.Snapshots
}

// manyFiles is n files under /workspace/src with one chunk each; version v rewrites file v-1, so each version differs from the one before in one file.
func manyFiles(n, v int) []manifestFile {
	files := []manifestFile{{Root: "workspace", Path: "src", Kind: kindDir, Mode: 0o755}}
	for i := range n {
		f := manifestFile{Root: "workspace", Path: fmt.Sprintf("src/f-%04d.go", i), Kind: kindFile, Mode: 0o644, MtimeMs: 1_790_000_000_000, Size: 4096, Chunks: hexIDs(fmt.Sprintf("f%d", i))}
		if v > 0 && i < v {
			f.MtimeMs += int64(v)
			f.Chunks = hexIDs(fmt.Sprintf("f%d-v%d", i, v))
		}
		files = append(files, f)
	}
	return files
}

func chunkSet(chunks []chunk) map[string]bool {
	out := map[string]bool{}
	for _, c := range chunks {
		out[c.ID] = true
	}
	return out
}

func idSet(ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func getIDs(call restoreCall) []string {
	var ids []string
	for _, g := range call.gets() {
		ids = append(ids, g.GetChunkId())
	}
	return ids
}
