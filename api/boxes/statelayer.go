package boxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/CMGS/cocoonbox/api/dialer"
	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/CMGS/cocoonbox/api/keyring"
	"github.com/CMGS/cocoonbox/api/store"
)

const maxBatchBytes = 512 << 20

var objectID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// StateAgents is the agents' State service as the control plane calls it.
type StateAgents interface {
	Plan(ctx context.Context, addr string, req *agentv1.PlanRequest) (*agentv1.PlanResponse, error)
	Upload(ctx context.Context, addr string, req *agentv1.UploadRequest, progress dialer.ProgressFunc) (*agentv1.TransferProgress, error)
	Restore(ctx context.Context, addr string, req *agentv1.RestoreRequest, progress dialer.ProgressFunc) (*agentv1.TransferProgress, error)
}

type chunk struct {
	ID   string `json:"id"`
	Size uint64 `json:"size"`
}

// snapshotRecord is the plaintext index entry of one snapshot; chunk ids are keyed hashes.
type snapshotRecord struct {
	ID            string  `json:"id"`
	Parent        string  `json:"parent,omitempty"`
	Consistency   string  `json:"consistency"`
	TakenAtMs     int64   `json:"taken_at_ms"`
	SavedAtMs     int64   `json:"saved_at_ms"`
	Files         uint64  `json:"files"`
	LogicalBytes  uint64  `json:"logical_bytes"`
	Chunks        []chunk `json:"chunks"`
	NewChunks     int     `json:"new_chunks"`
	UploadedBytes uint64  `json:"uploaded_bytes"`
}

// chunkIndex lists the chunks stored under one key; ids from another key never match.
type chunkIndex struct {
	KeyID  string   `json:"key_id"`
	Chunks []string `json:"chunks"`
}

type head struct {
	Latest    string `json:"latest"`
	SavedAtMs int64  `json:"saved_at_ms"`
}

// stateLayer moves user state between box agents and the store; one transfer per box at a time.
type stateLayer struct {
	agents StateAgents
	store  store.Store
	keys   keyring.Keyring

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	heads map[string]head
}

func newStateLayer(agents StateAgents, st store.Store, keys keyring.Keyring) *stateLayer {
	return &stateLayer{agents: agents, store: st, keys: keys, locks: map[string]*sync.Mutex{}, heads: map[string]head{}}
}

// save snapshots the state of box's agent at addr and uploads the chunks the store lacks.
func (l *stateLayer) save(ctx context.Context, box, addr string, consistency agentv1.Consistency, progress dialer.ProgressFunc) (*snapshotRecord, error) {
	unlock := l.lock(box)
	defer unlock()
	key, err := l.keys.Key(ctx, box)
	if err != nil {
		return nil, fmt.Errorf("state key: %w", err)
	}
	h, err := l.head(ctx, box)
	if err != nil {
		return nil, err
	}
	stateKey := &agentv1.StateKey{Key: key.Secret, KeyId: key.ID}
	plan, err := l.agents.Plan(ctx, addr, &agentv1.PlanRequest{Key: stateKey, Consistency: consistency, Parent: h.Latest})
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	if err = checkPlan(plan); err != nil {
		return nil, err
	}
	stored, err := l.index(ctx, box, key.ID)
	if err != nil {
		return nil, err
	}
	rec := &snapshotRecord{
		ID: plan.GetSnapshotId(), Parent: h.Latest, Consistency: consistency.String(), TakenAtMs: plan.GetTakenAtMs(),
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
	if err = l.upload(ctx, box, addr, rec, stateKey, missing, progress); err != nil {
		return nil, err
	}
	rec.SavedAtMs = time.Now().UnixMilli()
	if err := l.commit(ctx, box, key.ID, rec, stored); err != nil {
		return nil, err
	}
	return rec, nil
}

// upload sends missing in batches presigned just before each call, and the manifest with the last batch.
func (l *stateLayer) upload(ctx context.Context, box, addr string, rec *snapshotRecord, key *agentv1.StateKey, missing []chunk, progress dialer.ProgressFunc) error {
	var sent uint64
	all := batches(missing)
	for i, batch := range all {
		last := i == len(all)-1
		req := &agentv1.UploadRequest{SnapshotId: rec.ID, Key: key}
		for _, c := range batch {
			put, err := l.store.PresignPut(ctx, chunkKey(box, c.ID))
			if err != nil {
				return err
			}
			req.Puts = append(req.Puts, &agentv1.PresignedPut{ChunkId: c.ID, Url: put.URL, Headers: put.Headers})
		}
		if last {
			manifest, err := l.store.PresignPut(ctx, manifestKey(box, rec.ID))
			if err != nil {
				return err
			}
			req.Manifest = &agentv1.PresignedPut{ChunkId: rec.ID, Url: manifest.URL, Headers: manifest.Headers}
		}
		offset := sent
		final, err := l.agents.Upload(ctx, addr, req, func(p *agentv1.TransferProgress) {
			progress(&agentv1.TransferProgress{
				DoneBytes: offset + p.GetDoneBytes(), TotalBytes: rec.UploadedBytes,
				DoneChunks: p.GetDoneChunks(), TotalChunks: p.GetTotalChunks(),
				Finished: p.GetFinished() && last, ManifestId: p.GetManifestId(),
			})
		})
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		sent += batchBytes(batch)
		if last {
			if id := final.GetManifestId(); id != rec.ID {
				return fmt.Errorf("agent finished manifest %q, want %s", id, rec.ID)
			}
			return nil
		}
	}
	return nil
}

// restore writes snapshot id of box (the latest when empty) into the agent at addr.
func (l *stateLayer) restore(ctx context.Context, box, addr, id string, progress dialer.ProgressFunc) (*snapshotRecord, error) {
	unlock := l.lock(box)
	defer unlock()
	if id == "" {
		h, err := l.head(ctx, box)
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
	rec, err := l.record(ctx, box, id)
	if err != nil {
		return nil, err
	}
	key, err := l.keys.Key(ctx, box)
	if err != nil {
		return nil, fmt.Errorf("state key: %w", err)
	}
	manifest, err := l.store.PresignGet(ctx, manifestKey(box, id))
	if err != nil {
		return nil, err
	}
	req := &agentv1.RestoreRequest{
		Key:      &agentv1.StateKey{Key: key.Secret, KeyId: key.ID},
		Manifest: &agentv1.PresignedGet{ChunkId: id, Url: manifest.URL},
	}
	seen := map[string]bool{}
	for _, c := range rec.Chunks {
		if !objectID.MatchString(c.ID) {
			return nil, fmt.Errorf("snapshot %s lists invalid chunk id %q", id, c.ID)
		}
		if seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		get, err := l.store.PresignGet(ctx, chunkKey(box, c.ID))
		if err != nil {
			return nil, err
		}
		req.Gets = append(req.Gets, &agentv1.PresignedGet{ChunkId: c.ID, Url: get.URL})
	}
	if _, err := l.agents.Restore(ctx, addr, req, progress); err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	return rec, nil
}

// lastSaved returns when box's latest snapshot was saved, 0 when unknown.
func (l *stateLayer) lastSaved(ctx context.Context, box string) int64 {
	h, err := l.head(ctx, box)
	if err != nil {
		return 0
	}
	return h.SavedAtMs
}

// commit writes the snapshot record, then the index, then the head, so a crash leaves the previous head valid.
func (l *stateLayer) commit(ctx context.Context, box, keyID string, rec *snapshotRecord, stored map[string]bool) error {
	if err := putJSON(ctx, l.store, recordKey(box, rec.ID), rec); err != nil {
		return err
	}
	for _, c := range rec.Chunks {
		stored[c.ID] = true
	}
	ids := make([]string, 0, len(stored))
	for id := range stored {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if err := putJSON(ctx, l.store, indexKey(box), chunkIndex{KeyID: keyID, Chunks: ids}); err != nil {
		return err
	}
	h := head{Latest: rec.ID, SavedAtMs: rec.SavedAtMs}
	if err := putJSON(ctx, l.store, headKey(box), h); err != nil {
		return err
	}
	l.mu.Lock()
	l.heads[box] = h
	l.mu.Unlock()
	return nil
}

func (l *stateLayer) head(ctx context.Context, box string) (head, error) {
	l.mu.Lock()
	h, ok := l.heads[box]
	l.mu.Unlock()
	if ok {
		return h, nil
	}
	if err := getJSON(ctx, l.store, headKey(box), &h); err != nil && !errors.Is(err, store.ErrNotFound) {
		return head{}, err
	}
	l.mu.Lock()
	l.heads[box] = h
	l.mu.Unlock()
	return h, nil
}

func (l *stateLayer) index(ctx context.Context, box, keyID string) (map[string]bool, error) {
	var idx chunkIndex
	if err := getJSON(ctx, l.store, indexKey(box), &idx); err != nil && !errors.Is(err, store.ErrNotFound) {
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

func (l *stateLayer) record(ctx context.Context, box, id string) (*snapshotRecord, error) {
	var rec snapshotRecord
	if err := getJSON(ctx, l.store, recordKey(box, id), &rec); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", id, err)
	}
	return &rec, nil
}

func (l *stateLayer) lock(box string) func() {
	l.mu.Lock()
	m := l.locks[box]
	if m == nil {
		m = &sync.Mutex{}
		l.locks[box] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
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

// batches splits missing into runs of at most maxBatchBytes; nothing missing gives one empty batch for the manifest.
func batches(missing []chunk) [][]chunk {
	out := [][]chunk{nil}
	size := uint64(0)
	for _, c := range missing {
		cur := &out[len(out)-1]
		if len(*cur) > 0 && size+c.Size > maxBatchBytes {
			out = append(out, nil)
			cur, size = &out[len(out)-1], 0
		}
		*cur = append(*cur, c)
		size += c.Size
	}
	return out
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

func chunkKey(box, id string) string    { return box + "/chunks/" + id }
func manifestKey(box, id string) string { return box + "/manifests/" + id }
func recordKey(box, id string) string   { return box + "/snapshots/" + id + ".json" }
func indexKey(box string) string        { return box + "/index.json" }
func headKey(box string) string         { return box + "/head.json" }
