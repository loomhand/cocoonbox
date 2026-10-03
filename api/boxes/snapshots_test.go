package boxes

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/keyring"
	"github.com/CMGS/cocoonbox/api/machines"
)

func TestListSnapshotsNewestFirst(t *testing.T) {
	env := newStateEnv(t)
	p := statePath{user: alice, box: aliceBox}
	env.agent.chunks = hexIDs("c1")
	env.save(t)
	env.agent.chunks, env.agent.changed = hexIDs("c1", "c2"), new(uint64(1))
	env.save(t)
	for _, built := range []bool{false, true} {
		if built {
			env.collect(t, GCOptions{DeleteBatch: 1000}, 0)
			if !env.store.has(p.timeline()) {
				t.Fatal("the GC built no timeline")
			}
		}
		resp, err := env.svc.ListSnapshots(env.ctx, &controlv1.BoxRef{BoxId: aliceBox})
		if err != nil {
			t.Fatalf("ListSnapshots: %v", err)
		}
		got := resp.GetSnapshots()
		if len(got) != 2 || got[0].GetId() != snapshotID(2) || got[1].GetId() != snapshotID(1) {
			t.Fatalf("timeline built %v: snapshots = %v", built, got)
		}
		newest, oldest := got[0], got[1]
		if newest.ChangedFiles == nil || newest.GetChangedFiles() != 1 || oldest.ChangedFiles != nil {
			t.Errorf("changed files = %v and %v, want 1 and absent", newest.ChangedFiles, oldest.ChangedFiles)
		}
		if newest.GetAddedBytes() != env.agent.chunkSize() || oldest.GetAddedBytes() != env.agent.chunkSize() || !newest.GetQuiesced() || newest.GetFiles() != 7 || newest.GetSavedAtMs() < oldest.GetSavedAtMs() {
			t.Errorf("snapshot fields = %v", newest)
		}
	}
	other := auth.WithUser(t.Context(), "mallory")
	if _, err := env.svc.ListSnapshots(other, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("another user's ListSnapshots = %v, want PermissionDenied", err)
	}
}

func TestBrowseSnapshotListsOneDirectory(t *testing.T) {
	env := newStateEnv(t)
	p := statePath{user: alice, box: aliceBox}
	env.agent.files = sampleFiles()
	env.save(t)
	id := env.headID(t)
	browse := func(dir string) ([]string, error) {
		resp, err := env.svc.BrowseSnapshot(env.ctx, &controlv1.BrowseSnapshotRequest{BoxId: aliceBox, SnapshotId: id, Dir: dir})
		var out []string
		for _, e := range resp.GetEntries() {
			out = append(out, e.GetPath()+":"+strings.ToLower(strings.TrimPrefix(e.GetKind().String(), "ENTRY_KIND_")))
		}
		return out, err
	}
	gets := 0
	env.store.before = func(op, key string) {
		if op == "get" && key == p.manifest(id) {
			gets++
		}
	}
	for dir, want := range map[string][]string{
		"":                       {"/home/user:dir", "/workspace:dir"},
		"/home/user":             {"/home/user/Documents:dir"},
		"/home/user/Documents/":  {"/home/user/Documents/a.txt:file", "/home/user/Documents/link:symlink", "/home/user/Documents/sub:dir"},
		"/workspace/app":         {"/workspace/app/main.go:file"},
		"/workspace/app/main.go": nil,
	} {
		if got, err := browse(dir); err != nil || !slices.Equal(got, want) {
			t.Errorf("browse %q = %v, %v; want %v", dir, got, err, want)
		}
	}
	if gets != 1 {
		t.Errorf("the manifest was read %d times, want once for every directory", gets)
	}
	for _, dir := range []string{"/etc", "/home/user/../etc", "home/user", "/home/userx", "/workspace//app"} {
		if _, err := browse(dir); status.Code(err) != codes.InvalidArgument {
			t.Errorf("browse %q = %v, want InvalidArgument", dir, err)
		}
	}
	id = snapshotID(99)
	if _, err := browse(""); status.Code(err) != codes.NotFound {
		t.Errorf("browse of an unknown snapshot = %v, want NotFound", err)
	}
}

func TestRestorePathsPresignsOnlyTheirChunksAndPinsTheSnapshot(t *testing.T) {
	env := newStateEnv(t)
	p := statePath{user: alice, box: aliceBox}
	env.agent.files = sampleFiles()
	env.save(t)
	first := env.headID(t)
	env.agent.files = []manifestFile{{Root: "workspace", Path: "other", Kind: kindFile, Mode: 0o644, Size: 1, Chunks: hexIDs("c9")}}
	env.save(t)
	env.agent.restoreGate = make(chan struct{})

	op, err := env.svc.RestorePaths(env.ctx, &controlv1.RestorePathsRequest{BoxId: aliceBox, SnapshotId: first, Paths: []string{"/home/user/Documents/sub/", "/workspace/app/main.go"}})
	if err != nil {
		t.Fatalf("RestorePaths: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(env.agent.pathRestoreCalls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := env.svc.SaveState(env.ctx, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SaveState during a file restore = %v, want FailedPrecondition", err)
	}
	if _, _, err := env.svc.Resolve(env.ctx, alice, aliceBox); err != nil {
		t.Errorf("clients cannot reach the box during a file restore: %v", err)
	}
	env.collect(t, GCOptions{DeleteBatch: 1000}, 40*24*time.Hour)
	if !env.store.has(p.manifest(first)) || !env.store.has(p.chunk(hexID("b"))) {
		t.Error("the GC removed the snapshot a file restore reads")
	}
	close(env.agent.restoreGate)
	events := watch(t, env.svc, env.ctx, op.GetId(), "")
	last := events[len(events)-1]
	if last.GetPhase() != controlv1.Phase_PHASE_DONE || !strings.Contains(last.GetDetail(), "restored 2 files") {
		t.Fatalf("last event = %v", last)
	}
	call := env.agent.pathRestoreCalls()[0]
	scope := call.reqs[0].GetScope()
	target := time.UnixMilli(env.entry(t, first).SavedAtMs).UTC().Format(restoreTargetLayout)
	if !slices.Equal(scope.GetPaths(), []string{"/home/user/Documents/sub", "/workspace/app/main.go"}) || scope.GetTarget() != target {
		t.Errorf("scope = %v, want target %s", scope, target)
	}
	if !strings.Contains(last.GetDetail(), "~/Restored/"+target+"-2") {
		t.Errorf("detail %q does not name the agent's folder", last.GetDetail())
	}
	var presigned []string
	for _, g := range call.gets() {
		presigned = append(presigned, g.GetChunkId())
	}
	if want := hexIDs("b", "m1", "m2"); !slices.Equal(presigned, want) {
		t.Errorf("presigned %v, want only b, m1 and m2", presigned)
	}
	if call.reqs[0].GetManifest().GetChunkId() != first || len(call.reqs[0].GetKey().GetKey()) != keyring.KeySize {
		t.Errorf("first message = %v", call.reqs[0].GetManifest())
	}
	env.collect(t, GCOptions{DeleteBatch: 1000}, 40*24*time.Hour)
	if env.store.has(p.manifest(first)) {
		t.Error("the GC kept the snapshot after the restore ended")
	}
}

func TestRestorePathsRefusals(t *testing.T) {
	env := newStateEnv(t)
	env.agent.files = sampleFiles()
	env.save(t)
	id := env.headID(t)
	restore := func(ctx context.Context, snapshot string, paths ...string) (*controlv1.Operation, error) {
		return env.svc.RestorePaths(ctx, &controlv1.RestorePathsRequest{BoxId: aliceBox, SnapshotId: snapshot, Paths: paths})
	}
	for _, paths := range [][]string{nil, {"/etc/passwd"}, {"/workspace/../etc"}, {"relative"}, {"/home/user/./x"}} {
		if _, err := restore(env.ctx, id, paths...); status.Code(err) != codes.InvalidArgument {
			t.Errorf("paths %q: %v, want InvalidArgument", paths, err)
		}
	}
	if _, err := restore(env.ctx, snapshotID(99), "/workspace"); status.Code(err) != codes.NotFound {
		t.Errorf("unknown snapshot: %v, want NotFound", err)
	}
	if _, err := restore(auth.WithUser(t.Context(), "mallory"), id, "/workspace"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("another user: %v, want PermissionDenied", err)
	}

	op, err := restore(env.ctx, id, "/workspace/missing")
	if err != nil {
		t.Fatalf("RestorePaths: %v", err)
	}
	if last := env.finalEvent(t, op); last.GetPhase() != controlv1.Phase_PHASE_FAILED || !strings.Contains(last.GetDetail(), "/workspace/missing is not in the snapshot") {
		t.Errorf("a path the snapshot lacks ended with %v", last)
	}
	env.agent.restoreErr = status.Error(codes.Unimplemented, "unknown method RestorePaths")
	op, err = restore(env.ctx, id, "/workspace")
	if err != nil {
		t.Fatalf("RestorePaths: %v", err)
	}
	if last := env.finalEvent(t, op); last.GetPhase() != controlv1.Phase_PHASE_FAILED || !strings.Contains(last.GetDetail(), "upgrade the box") {
		t.Errorf("an old agent ended with %v", last)
	}
	if n := len(env.agent.pathRestoreCalls()); n != 1 {
		t.Errorf("agent got %d file restores, want only the old agent's", n)
	}

	env.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Revision: 9})
	if _, err := restore(env.ctx, id, "/workspace"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("hibernated box: %v, want FailedPrecondition", err)
	}
}

func TestOpenSealedRejectsTamperingAndOtherIDs(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	id, sealed := sealManifest(secret, sampleFiles())
	if _, err := openSealed(secret, id, sealed, manifestLimit); err != nil {
		t.Fatalf("open: %v", err)
	}
	flipped := slices.Clone(sealed)
	flipped[len(flipped)-1] ^= 1
	if _, err := openSealed(secret, id, flipped, manifestLimit); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Errorf("tampered object: %v", err)
	}
	if _, err := openSealed(secret, snapshotID(1), sealed, manifestLimit); err == nil {
		t.Error("an object opened under another id")
	}
	if _, err := openSealed([]byte("another key, also 32 bytes long."), id, sealed, manifestLimit); err == nil {
		t.Error("an object opened under another key")
	}
	if _, err := openSealed(secret, id, sealed, 10); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized object: %v", err)
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

func (a *fakeAgent) pathRestoreCalls() []restoreCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.pathRestores)
}

func sampleFiles() []manifestFile {
	return []manifestFile{
		{Root: "home", Path: "Documents", Kind: kindDir, Mode: 0o755},
		{Root: "home", Path: "Documents/a.txt", Kind: kindFile, Mode: 0o644, Size: 5, Chunks: hexIDs("a")},
		{Root: "home", Path: "Documents/link", Kind: kindSymlink, Mode: 0o777},
		{Root: "home", Path: "Documents/sub", Kind: kindDir, Mode: 0o500},
		{Root: "home", Path: "Documents/sub/b", Kind: kindFile, Mode: 0o600, Size: 7, Chunks: hexIDs("b")},
		{Root: "workspace", Path: "app", Kind: kindDir, Mode: 0o755},
		{Root: "workspace", Path: "app/main.go", Kind: kindFile, Mode: 0o644, Size: 9, Chunks: hexIDs("m1", "m2", "m1")},
	}
}

// sealManifest seals files as the agent does and returns the snapshot id with the sealed object.
func sealManifest(secret []byte, files []manifestFile) (string, []byte) {
	plain, err := json.Marshal(manifest{Version: manifestVersion, Files: files})
	if err != nil {
		panic(err)
	}
	id := hex.EncodeToString(mac(mac(secret, idLabel), plain))
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		panic(err)
	}
	packed := enc.EncodeAll(plain, nil)
	block, err := aes.NewCipher(mac(secret, sealLabel))
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	nonce := make([]byte, nonceSize)
	_, _ = rand.Read(nonce)
	return id, gcm.Seal(nonce, nonce, packed, []byte(id))
}
