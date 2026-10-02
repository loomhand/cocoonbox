package boxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/dialer"
	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/CMGS/cocoonbox/api/keyring"
	"github.com/CMGS/cocoonbox/api/machines"
	"github.com/CMGS/cocoonbox/api/store"
)

const (
	maxBatchBytes = 512 << 20
	// pageSize keeps a control message near 1 MB whatever the state size.
	pageSize = 2048

	commitAttempts   = 3
	commitRetryDelay = time.Second
)

var objectID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// StateAgents is the agents' State service as the control plane calls it.
type StateAgents interface {
	Plan(ctx context.Context, ep machines.Endpoint, req *agentv1.PlanRequest) (*agentv1.PlanResponse, error)
	Upload(ctx context.Context, ep machines.Endpoint, req *agentv1.UploadRequest, progress dialer.ProgressFunc) (*agentv1.TransferProgress, error)
	Restore(ctx context.Context, ep machines.Endpoint, reqs iter.Seq2[*agentv1.RestoreRequest, error], progress dialer.ProgressFunc) (*agentv1.TransferProgress, error)
}

type chunk struct {
	ID   string `json:"id"`
	Size uint64 `json:"size"`
}

// timelineEntry describes one kept snapshot without its chunk list.
type timelineEntry struct {
	ID            string `json:"id"`
	Consistency   string `json:"consistency"`
	TakenAtMs     int64  `json:"taken_at_ms"`
	SavedAtMs     int64  `json:"saved_at_ms"`
	Files         uint64 `json:"files"`
	LogicalBytes  uint64 `json:"logical_bytes"`
	NewChunks     int    `json:"new_chunks"`
	UploadedBytes uint64 `json:"uploaded_bytes"`
}

// snapshotRecord is the plaintext index entry of one snapshot; chunk ids are keyed hashes.
type snapshotRecord struct {
	timelineEntry
	// Parent is the head the save planned on; pruning may remove it, and nothing follows it.
	Parent string  `json:"parent,omitempty"`
	Chunks []chunk `json:"chunks"`
}

// timeline lists a box's kept snapshots; the GC prunes it and every reader takes the kept set from it.
type timeline struct {
	Snapshots []timelineEntry `json:"snapshots"`
}

// chunkIndex lists the chunks stored under one user's key; ids from another key never match.
type chunkIndex struct {
	KeyID  string   `json:"key_id"`
	Chunks []string `json:"chunks"`
}

type head struct {
	Latest    string `json:"latest"`
	SavedAtMs int64  `json:"saved_at_ms"`
}

// statePath places a box's objects in the store: chunks and their index per user, snapshots per box.
type statePath struct {
	user string
	box  string
}

func (p statePath) chunk(id string) string    { return p.chunks() + id }
func (p statePath) chunks() string            { return p.user + "/chunks/" }
func (p statePath) index() string             { return p.user + "/index.json" }
func (p statePath) manifest(id string) string { return p.manifests() + id }
func (p statePath) manifests() string         { return p.boxDir() + "manifests/" }
func (p statePath) record(id string) string   { return p.records() + id + ".json" }
func (p statePath) records() string           { return p.boxDir() + "snapshots/" }
func (p statePath) timeline() string          { return p.boxDir() + "timeline.json" }
func (p statePath) head() string              { return p.boxDir() + "head.json" }
func (p statePath) boxDir() string            { return p.boxes() + p.box + "/" }
func (p statePath) boxes() string             { return p.user + "/boxes/" }

// stateLayer moves user state between box agents and the store; one transfer per box at a time.
type stateLayer struct {
	agents     StateAgents
	store      store.Store
	keys       keyring.Keyring
	retryDelay time.Duration

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	users map[string]*sync.RWMutex
	heads map[statePath]head
	pins  map[statePath]map[string]int
}

func newStateLayer(agents StateAgents, st store.Store, keys keyring.Keyring) *stateLayer {
	return &stateLayer{
		agents: agents, store: st, keys: keys, retryDelay: commitRetryDelay,
		locks: map[string]*sync.Mutex{}, users: map[string]*sync.RWMutex{}, heads: map[statePath]head{}, pins: map[statePath]map[string]int{},
	}
}

// save snapshots the state of the agent at ep and uploads the chunks the user's store lacks.
func (l *stateLayer) save(ctx context.Context, p statePath, ep machines.Endpoint, consistency agentv1.Consistency, progress dialer.ProgressFunc) (*snapshotRecord, error) {
	unlock := l.lock(p.box)
	defer unlock()
	// The GC sweep takes this lock exclusively, so no save is between its index read and its commit.
	user := l.userLock(p.user)
	user.RLock()
	defer user.RUnlock()
	key, err := l.keys.StateKey(ctx, p.user)
	if err != nil {
		return nil, fmt.Errorf("state key: %w", err)
	}
	h, err := l.head(ctx, p)
	if err != nil {
		return nil, err
	}
	stateKey := &agentv1.StateKey{Key: key.Secret, KeyId: key.ID}
	rec, err := l.snapshot(ctx, p, ep, stateKey, consistency, h.Latest, progress)
	if h.Latest != "" && status.Code(err) == codes.InvalidArgument {
		log.WithFunc("boxes.stateLayer.save").WithField("box", p.box).Warnf(ctx, "agent refused a plan on parent %s, planning without it: %v", h.Latest, err)
		rec, err = l.snapshot(ctx, p, ep, stateKey, consistency, "", progress)
	}
	if err != nil {
		return nil, err
	}
	rec.Parent = h.Latest
	rec.SavedAtMs = time.Now().UnixMilli()
	if err := l.commit(ctx, p, key.ID, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func (l *stateLayer) snapshot(ctx context.Context, p statePath, ep machines.Endpoint, key *agentv1.StateKey, consistency agentv1.Consistency, parent string, progress dialer.ProgressFunc) (*snapshotRecord, error) {
	plan, err := l.agents.Plan(ctx, ep, &agentv1.PlanRequest{Key: key, Consistency: consistency, Parent: parent})
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	if err = checkPlan(plan); err != nil {
		return nil, err
	}
	stored, err := l.index(ctx, p, key.GetKeyId())
	if err != nil {
		return nil, err
	}
	rec := &snapshotRecord{
		ID: plan.GetSnapshotId(), Consistency: consistency.String(), TakenAtMs: plan.GetTakenAtMs(),
		Files: plan.GetFiles(), LogicalBytes: plan.GetLogicalBytes(),
	}
	var missing []chunk
	seen := map[string]bool{}
	for _, c := range plan.GetChunks() {
		rec.Chunks = append(rec.Chunks, chunk{ID: c.GetId(), Size: c.GetSize()})
		if stored[c.GetId()] || seen[c.GetId()] {
			continue
		}
		seen[c.GetId()] = true
		missing = append(missing, chunk{ID: c.GetId(), Size: c.GetSize()})
		rec.NewChunks++
		rec.UploadedBytes += c.GetSize()
	}
	if err = l.upload(ctx, p, ep, rec, key, missing, progress); err != nil {
		return nil, err
	}
	return rec, nil
}

// upload sends missing in batches presigned just before each call, and the manifest with the last batch.
func (l *stateLayer) upload(ctx context.Context, p statePath, ep machines.Endpoint, rec *snapshotRecord, key *agentv1.StateKey, missing []chunk, progress dialer.ProgressFunc) error {
	var sent uint64
	all := batches(missing)
	for i, batch := range all {
		last := i == len(all)-1
		req := &agentv1.UploadRequest{SnapshotId: rec.ID, Key: key}
		for _, c := range batch {
			put, err := l.store.PresignPut(ctx, p.chunk(c.ID))
			if err != nil {
				return err
			}
			req.Puts = append(req.Puts, &agentv1.PresignedPut{ChunkId: c.ID, Url: put.URL, Headers: put.Headers})
		}
		if last {
			manifest, err := l.store.PresignPut(ctx, p.manifest(rec.ID))
			if err != nil {
				return err
			}
			req.Manifest = &agentv1.PresignedPut{ChunkId: rec.ID, Url: manifest.URL, Headers: manifest.Headers}
		}
		offset, size := sent, batchBytes(batch)
		final, err := l.agents.Upload(ctx, ep, req, plaintextProgress(size, func(p *agentv1.TransferProgress) {
			p.DoneBytes += offset
			p.TotalBytes = rec.UploadedBytes
			p.Finished = p.Finished && last
			progress(p)
		}))
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		sent += size
		if last {
			if id := final.GetManifestId(); id != rec.ID {
				return fmt.Errorf("agent finished manifest %q, want %s", id, rec.ID)
			}
			return nil
		}
	}
	return nil
}

// restore writes snapshot id of p's box (the latest when empty) into the agent at ep.
func (l *stateLayer) restore(ctx context.Context, p statePath, ep machines.Endpoint, id string, progress dialer.ProgressFunc) (*snapshotRecord, error) {
	unlock := l.lock(p.box)
	defer unlock()
	if id == "" {
		h, err := l.head(ctx, p)
		if err != nil {
			return nil, err
		}
		if h.Latest == "" {
			return nil, errors.New("box has no saved state")
		}
		id = h.Latest
	}
	if !objectID.MatchString(id) {
		return nil, fmt.Errorf("invalid snapshot id %q", id)
	}
	rec, err := l.record(ctx, p, id)
	if err != nil {
		return nil, err
	}
	key, err := l.keys.StateKey(ctx, p.user)
	if err != nil {
		return nil, fmt.Errorf("state key: %w", err)
	}
	manifest, err := l.store.PresignGet(ctx, p.manifest(id))
	if err != nil {
		return nil, err
	}
	first := &agentv1.RestoreRequest{
		Key:      &agentv1.StateKey{Key: key.Secret, KeyId: key.ID},
		Manifest: &agentv1.PresignedGet{ChunkId: id, Url: manifest.URL},
	}
	seen := map[string]bool{}
	var ids []string
	var total uint64
	for _, c := range rec.Chunks {
		if !objectID.MatchString(c.ID) {
			return nil, fmt.Errorf("snapshot %s lists invalid chunk id %q", id, c.ID)
		}
		if seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		total += c.Size
		ids = append(ids, c.ID)
	}
	if _, err := l.agents.Restore(ctx, ep, l.restorePages(ctx, p, first, ids), plaintextProgress(total, progress)); err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	return rec, nil
}

// restorePages yields first, then ids in pages of presigned GETs, each presigned just before it is sent.
func (l *stateLayer) restorePages(ctx context.Context, p statePath, first *agentv1.RestoreRequest, ids []string) iter.Seq2[*agentv1.RestoreRequest, error] {
	return func(yield func(*agentv1.RestoreRequest, error) bool) {
		if !yield(first, nil) {
			return
		}
		for page := range slices.Chunk(ids, pageSize) {
			req := &agentv1.RestoreRequest{Gets: make([]*agentv1.PresignedGet, 0, len(page))}
			for _, id := range page {
				get, err := l.store.PresignGet(ctx, p.chunk(id))
				if err != nil {
					yield(nil, err)
					return
				}
				req.Gets = append(req.Gets, &agentv1.PresignedGet{ChunkId: id, Url: get.URL})
			}
			if !yield(req, nil) {
				return
			}
		}
	}
}

// lastSaved returns when p's latest snapshot was saved, 0 when unknown.
func (l *stateLayer) lastSaved(ctx context.Context, p statePath) int64 {
	h, err := l.head(ctx, p)
	if err != nil {
		return 0
	}
	return h.SavedAtMs
}

// commit adds the uploaded chunks to the index, then writes the snapshot record, the timeline and the head, so a crash leaves the previous head valid.
func (l *stateLayer) commit(ctx context.Context, p statePath, keyID string, rec *snapshotRecord) error {
	if err := l.retry(ctx, func() error { return l.addToIndex(ctx, p, keyID, rec.Chunks) }); err != nil {
		return err
	}
	if err := l.retry(ctx, func() error { return putJSON(ctx, l.store, p.record(rec.ID), rec) }); err != nil {
		return err
	}
	if err := l.retry(ctx, func() error { return l.addToTimeline(ctx, p, rec.timelineEntry) }); err != nil {
		return err
	}
	h := head{Latest: rec.ID, SavedAtMs: rec.SavedAtMs}
	if err := l.retry(ctx, func() error { return putJSON(ctx, l.store, p.head(), h) }); err != nil {
		return err
	}
	l.mu.Lock()
	l.heads[p] = h
	l.mu.Unlock()
	return nil
}

// addToIndex merges chunks into the user's index under a per-user lock, since every box of the user writes it.
func (l *stateLayer) addToIndex(ctx context.Context, p statePath, keyID string, chunks []chunk) error {
	unlock := l.lock(p.index())
	defer unlock()
	stored, err := l.index(ctx, p, keyID)
	if err != nil {
		return err
	}
	for _, c := range chunks {
		stored[c.ID] = true
	}
	return putJSON(ctx, l.store, p.index(), chunkIndex{KeyID: keyID, Chunks: slices.Sorted(maps.Keys(stored))})
}

// addToTimeline appends e to p's timeline; a box without one gets it from the GC, which builds it from the records.
func (l *stateLayer) addToTimeline(ctx context.Context, p statePath, e timelineEntry) error {
	var tl timeline
	err := getJSON(ctx, l.store, p.timeline(), &tl)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil || slices.ContainsFunc(tl.Snapshots, func(x timelineEntry) bool { return x.ID == e.ID }) {
		return err
	}
	tl.Snapshots = append(tl.Snapshots, e)
	return putJSON(ctx, l.store, p.timeline(), tl)
}

// pin keeps snapshot id of p's box from pruning until the returned func runs.
func (l *stateLayer) pin(p statePath, id string) func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pins[p] == nil {
		l.pins[p] = map[string]int{}
	}
	l.pins[p][id]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.pins[p][id]--; l.pins[p][id] == 0 {
			delete(l.pins[p], id)
		}
	}
}

func (l *stateLayer) pinned(p statePath) map[string]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]bool{}
	for id := range l.pins[p] {
		out[id] = true
	}
	return out
}

func (l *stateLayer) retry(ctx context.Context, fn func() error) error {
	err := fn()
	for attempt := 1; err != nil && attempt < commitAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return err
		case <-time.After(l.retryDelay * time.Duration(attempt)):
		}
		err = fn()
	}
	return err
}

func (l *stateLayer) head(ctx context.Context, p statePath) (head, error) {
	l.mu.Lock()
	h, ok := l.heads[p]
	l.mu.Unlock()
	if ok {
		return h, nil
	}
	if err := getJSON(ctx, l.store, p.head(), &h); err != nil && !errors.Is(err, store.ErrNotFound) {
		return head{}, err
	}
	l.mu.Lock()
	l.heads[p] = h
	l.mu.Unlock()
	return h, nil
}

func (l *stateLayer) index(ctx context.Context, p statePath, keyID string) (map[string]bool, error) {
	var idx chunkIndex
	if err := getJSON(ctx, l.store, p.index(), &idx); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	stored := map[string]bool{}
	if idx.KeyID != keyID {
		return stored, nil
	}
	for _, id := range idx.Chunks {
		stored[id] = true
	}
	return stored, nil
}

func (l *stateLayer) record(ctx context.Context, p statePath, id string) (*snapshotRecord, error) {
	var rec snapshotRecord
	if err := getJSON(ctx, l.store, p.record(id), &rec); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", id, err)
	}
	return &rec, nil
}

func (l *stateLayer) lock(name string) func() {
	l.mu.Lock()
	m := l.locks[name]
	if m == nil {
		m = &sync.Mutex{}
		l.locks[name] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// userLock returns the lock that saves of user's boxes hold shared and the GC sweep holds exclusively.
func (l *stateLayer) userLock(user string) *sync.RWMutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.users[user]
	if m == nil {
		m = &sync.RWMutex{}
		l.users[user] = m
	}
	return m
}

func checkPlan(plan *agentv1.PlanResponse) error {
	if !objectID.MatchString(plan.GetSnapshotId()) {
		return fmt.Errorf("agent returned invalid snapshot id %q", plan.GetSnapshotId())
	}
	for _, c := range plan.GetChunks() {
		if !objectID.MatchString(c.GetId()) {
			return fmt.Errorf("agent returned invalid chunk id %q", c.GetId())
		}
	}
	return nil
}

// batches splits missing into runs of at most pageSize chunks and maxBatchBytes; nothing missing gives one empty batch for the manifest.
func batches(missing []chunk) [][]chunk {
	out := [][]chunk{nil}
	size := uint64(0)
	for _, c := range missing {
		cur := &out[len(out)-1]
		if len(*cur) > 0 && (len(*cur) == pageSize || size+c.Size > maxBatchBytes) {
			out = append(out, nil)
			cur, size = &out[len(out)-1], 0
		}
		*cur = append(*cur, c)
		size += c.Size
	}
	return out
}

func plaintextProgress(size uint64, progress dialer.ProgressFunc) dialer.ProgressFunc {
	return func(p *agentv1.TransferProgress) {
		done := size
		if p.GetTotalChunks() > 0 {
			done = size * uint64(min(p.GetDoneChunks(), p.GetTotalChunks())) / uint64(p.GetTotalChunks())
		}
		progress(&agentv1.TransferProgress{
			DoneBytes: done, TotalBytes: size, DoneChunks: p.GetDoneChunks(), TotalChunks: p.GetTotalChunks(),
			Finished: p.GetFinished(), ManifestId: p.GetManifestId(),
		})
	}
}

func batchBytes(batch []chunk) uint64 {
	var n uint64
	for _, c := range batch {
		n += c.Size
	}
	return n
}

func putJSON(ctx context.Context, st store.Store, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	return st.Put(ctx, key, raw)
}

func getJSON(ctx context.Context, st store.Store, key string, v any) error {
	raw, err := st.Get(ctx, key)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode %s: %w", key, err)
	}
	return nil
}
