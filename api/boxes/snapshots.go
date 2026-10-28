package boxes

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/dialer"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/store"
)

const (
	browseTTL   = 10 * time.Minute
	browseLimit = 5000
	// restoreTargetLayout names the folder under ~/Restored after the snapshot's save time.
	restoreTargetLayout = "2006-01-02T1504Z"
)

var (
	errNotKept = errors.New("snapshot is not kept")

	entryKinds = map[string]controlv1.EntryKind{
		kindFile: controlv1.EntryKind_ENTRY_KIND_FILE, kindDir: controlv1.EntryKind_ENTRY_KIND_DIR, kindSymlink: controlv1.EntryKind_ENTRY_KIND_SYMLINK,
	}
)

// browsed is a snapshot's directory index, kept per box for browseTTL so browsing does not decrypt the manifest per directory.
type browsed struct {
	id   string
	at   time.Time
	dirs map[string][]manifestFile
}

// restoredPaths summarizes a finished RestorePaths.
type restoredPaths struct {
	files  int
	bytes  uint64
	target string
}

func (s *Service) ListSnapshots(ctx context.Context, ref *controlv1.BoxRef) (*controlv1.ListSnapshotsResponse, error) {
	box := ref.GetBoxId()
	user, err := s.ownerOf(ctx, box)
	if err != nil {
		return nil, err
	}
	if s.state == nil {
		return nil, status.Error(codes.FailedPrecondition, "state store is not configured")
	}
	entries, err := s.state.kept(ctx, statePath{user: user, box: box})
	if err != nil {
		return nil, serviceError(ctx, log.WithFunc("boxes.Service.ListSnapshots").WithField("box", box), err, "list snapshots")
	}
	resp := &controlv1.ListSnapshotsResponse{}
	for _, e := range slices.Backward(entries) {
		resp.Snapshots = append(resp.Snapshots, &controlv1.Snapshot{
			Id: e.ID, SavedAtMs: e.SavedAtMs, Quiesced: e.Consistency == agentv1.Consistency_CONSISTENCY_QUIESCED.String(),
			Files: e.Files, LogicalBytes: e.LogicalBytes, ChangedFiles: e.ChangedFiles, AddedBytes: e.UploadedBytes,
		})
	}
	return resp, nil
}

func (s *Service) BrowseSnapshot(ctx context.Context, req *controlv1.BrowseSnapshotRequest) (*controlv1.BrowseSnapshotResponse, error) {
	box := req.GetBoxId()
	user, err := s.ownerOf(ctx, box)
	if err != nil {
		return nil, err
	}
	if s.state == nil {
		return nil, status.Error(codes.FailedPrecondition, "state store is not configured")
	}
	dir := req.GetDir()
	if dir != "" {
		bp, perr := parseBoxPath(dir)
		if perr != nil {
			return nil, status.Error(codes.InvalidArgument, perr.Error())
		}
		dir = bp.String()
	}
	dirs, err := s.state.browse(ctx, statePath{user: user, box: box}, req.GetSnapshotId())
	if err != nil {
		return nil, snapshotError(ctx, box, err, "browse snapshot")
	}
	list := dirs[dir]
	resp := &controlv1.BrowseSnapshotResponse{Truncated: len(list) > browseLimit}
	for _, f := range list[:min(len(list), browseLimit)] {
		resp.Entries = append(resp.Entries, &controlv1.SnapshotEntry{Path: f.boxPath(), Kind: entryKinds[f.Kind], Size: f.Size, MtimeMs: f.MtimeMs})
	}
	return resp, nil
}

func (s *Service) RestorePaths(ctx context.Context, req *controlv1.RestorePathsRequest) (*controlv1.Operation, error) {
	box := req.GetBoxId()
	user, err := s.ownerOf(ctx, box)
	if err != nil {
		return nil, err
	}
	if len(req.GetPaths()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no path to restore")
	}
	unlock, err := s.lockShared(ctx, user)
	if err != nil {
		return nil, err
	}
	defer unlock()
	paths := make([]boxPath, 0, len(req.GetPaths()))
	for _, raw := range req.GetPaths() {
		bp, perr := parseBoxPath(raw)
		if perr != nil {
			return nil, status.Error(codes.InvalidArgument, perr.Error())
		}
		paths = append(paths, bp)
	}
	m, err := s.runningMachine(ctx, box, user)
	if err != nil {
		return nil, err
	}
	p := statePath{user: user, box: box}
	e, err := s.state.keptEntry(ctx, p, req.GetSnapshotId())
	if err != nil {
		return nil, snapshotError(ctx, box, err, "read snapshot")
	}
	op, started := s.ops.start(context.WithoutCancel(ctx), user, box, controlv1.Phase_PHASE_RESTORING_FILES, m.Revision, stateTimeout)
	if started {
		go s.runRestorePaths(op, user, m, e, paths)
	}
	return &controlv1.Operation{Id: op.id, BoxId: box}, nil
}

// runRestorePaths copies paths of snapshot e into the running machine m and pins e until it ends.
func (s *Service) runRestorePaths(op *operation, user string, m machines.Machine, e timelineEntry, paths []boxPath) {
	logger := log.WithFunc("boxes.Service.runRestorePaths").WithField("box", op.boxID)
	p := statePath{user: user, box: op.boxID}
	unpin, err := s.state.pin(op.ctx, p, e.ID)
	defer unpin()
	if err != nil {
		logger.Error(op.ctx, err, "pin the snapshot")
		op.fail("pin the snapshot failed")
		return
	}
	savedAt := time.UnixMilli(e.SavedAtMs).UTC().Format(time.RFC3339)
	op.note(controlv1.Phase_PHASE_RESTORING_FILES, controlv1.BoxState_BOX_STATE_RUNNING, fmt.Sprintf("copying %d paths of the snapshot saved at %s", len(paths), savedAt))
	ep, err := s.machines.Address(op.ctx, m.ID)
	var done restoredPaths
	if err == nil {
		done, err = s.state.restorePaths(s.agentContext(op.ctx, m), p, ep, e, paths, progressNotes(op, controlv1.Phase_PHASE_RESTORING_FILES, "restoring"))
	}
	switch {
	case status.Code(err) == codes.Unimplemented:
		logger.Error(op.ctx, err, "restore paths")
		op.fail("this box's agent cannot restore files; upgrade the box first")
	case errors.Is(err, errNotKept), errors.Is(err, errBadPath):
		op.fail(err.Error())
	case err != nil:
		logger.Error(op.ctx, err, "restore paths")
		op.fail("restore failed")
	default:
		op.succeed(fmt.Sprintf("restored %d files (%.1f MiB) from the snapshot saved at %s into ~/Restored/%s", done.files, float64(done.bytes)/mib, savedAt, done.target))
	}
}

// kept returns the entries of p's kept snapshots, oldest first; a box without a timeline lists its records.
func (l *stateLayer) kept(ctx context.Context, p statePath) ([]timelineEntry, error) {
	tl, found, err := l.readTimeline(ctx, p)
	if err != nil || found {
		return tl.Snapshots, err
	}
	built, err := l.readEntries(ctx, p, nil)
	if err != nil {
		return nil, err
	}
	return slices.SortedFunc(maps.Values(built), compareEntries), nil
}

// keptEntry returns the entry of snapshot id when p's box keeps it.
func (l *stateLayer) keptEntry(ctx context.Context, p statePath, id string) (timelineEntry, error) {
	entries, err := l.kept(ctx, p)
	if err != nil {
		return timelineEntry{}, err
	}
	i := slices.IndexFunc(entries, func(e timelineEntry) bool { return e.ID == id })
	if i < 0 {
		return timelineEntry{}, fmt.Errorf("%w: %q", errNotKept, id)
	}
	return entries[i], nil
}

// browse returns the directory index of kept snapshot id of p's box, from the cache when fresh.
func (l *stateLayer) browse(ctx context.Context, p statePath, id string) (map[string][]manifestFile, error) {
	l.mu.Lock()
	maps.DeleteFunc(l.browsed, func(_ statePath, b browsed) bool { return time.Since(b.at) > browseTTL })
	cached, ok := l.browsed[p]
	l.mu.Unlock()
	if ok && cached.id == id {
		return cached.dirs, nil
	}
	if _, err := l.keptEntry(ctx, p, id); err != nil {
		return nil, err
	}
	key, err := l.keys.StateKey(ctx, p.user)
	if err != nil {
		return nil, fmt.Errorf("state key: %w", err)
	}
	m, err := l.openManifest(ctx, p, key, id)
	if err != nil {
		return nil, err
	}
	dirs := m.tree()
	l.mu.Lock()
	l.browsed[p] = browsed{id: id, at: time.Now(), dirs: dirs}
	l.mu.Unlock()
	return dirs, nil
}

// restorePaths copies paths of snapshot e, which the caller pins, into the agent at ep; it presigns only the chunks of those paths that the snapshot's record lists.
func (l *stateLayer) restorePaths(ctx context.Context, p statePath, ep machines.Endpoint, e timelineEntry, paths []boxPath, progress dialer.ProgressFunc) (restoredPaths, error) {
	var done restoredPaths
	// The caller's pin keeps e from a prune that starts later; the box lock orders this check after a prune already running.
	key, err := l.keys.StateKey(ctx, p.user)
	if err != nil {
		return done, fmt.Errorf("state key: %w", err)
	}
	unlock := l.locks.Lock(p.box)
	_, err = l.keptEntry(ctx, p, e.ID)
	if err == nil {
		err = l.materialize(ctx, p, key, e.ID)
	}
	unlock()
	if err != nil {
		return done, err
	}
	m, err := l.openManifest(ctx, p, key, e.ID)
	if err != nil {
		return done, err
	}
	files, ids, err := m.selectPaths(paths)
	if err != nil {
		return done, err
	}
	rec, err := l.record(ctx, p, e.ID)
	if err != nil {
		return done, err
	}
	sizes := map[string]uint64{}
	for _, c := range rec.Chunks {
		sizes[c.ID] = c.Size
	}
	var total uint64
	for _, id := range ids {
		size, ok := sizes[id]
		if !ok || !objectID.MatchString(id) {
			return done, fmt.Errorf("manifest %s names chunk %q that its record lacks", e.ID, id)
		}
		total += size
	}
	for _, f := range files {
		if f.Kind == kindFile {
			done.files++
			done.bytes += f.Size
		}
	}
	manifestURL, err := l.store.PresignGet(ctx, p.manifest(e.ID))
	if err != nil {
		return done, err
	}
	scope := &agentv1.RestoreScope{Target: time.UnixMilli(e.SavedAtMs).UTC().Format(restoreTargetLayout)}
	for _, bp := range paths {
		scope.Paths = append(scope.Paths, bp.String())
	}
	first := &agentv1.RestoreRequest{
		Key:      &agentv1.StateKey{Key: key.Secret, KeyId: key.ID},
		Manifest: &agentv1.PresignedGet{ChunkId: e.ID, Url: manifestURL.URL},
		Scope:    scope,
	}
	log.WithFunc("boxes.stateLayer.restorePaths").WithField("box", p.box).Infof(ctx, "restoring %s of snapshot %s: %d entries, %d files, %d chunks presigned",
		strings.Join(scope.Paths, ", "), e.ID, len(files), done.files, len(ids))
	final, err := l.agents.RestorePaths(ctx, p.box, ep, l.restorePages(ctx, p, first, ids), plaintextProgress(total, progress))
	if err != nil {
		return done, fmt.Errorf("restore paths: %w", err)
	}
	done.target = final.GetTarget()
	return done, nil
}

func snapshotError(ctx context.Context, box string, err error, action string) error {
	switch {
	case errors.Is(err, errNotKept), errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, "snapshot not found")
	default:
		return serviceError(ctx, log.WithFunc("boxes.snapshotError").WithField("box", box), err, action)
	}
}
