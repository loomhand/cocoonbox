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
	stored, err := l.index(ctx, box)
	if err != nil {
		return nil, err
	}
	rec := &snapshotRecord{
		ID: plan.GetSnapshotId(), Parent: h.Latest, Consistency: consistency.String(), TakenAtMs: plan.GetTakenAtMs(),
		Files: plan.GetFiles(), LogicalBytes: plan.GetLogicalBytes(),
	}
	req := &agentv1.UploadRequest{SnapshotId: rec.ID, Key: stateKey}
	seen := map[string]bool{}
	for _, c := range plan.GetChunks() {
		rec.Chunks = append(rec.Chunks, chunk{ID: c.GetId(), Size: c.GetSize()})
		if stored[c.GetId()] || seen[c.GetId()] {
			continue
		}
		seen[c.GetId()] = true
		put, presignErr := l.store.PresignPut(ctx, chunkKey(box, c.GetId()))
		if presignErr != nil {
			return nil, presignErr
		}
		req.Puts = append(req.Puts, &agentv1.PresignedPut{ChunkId: c.GetId(), Url: put.URL, Headers: put.Headers})
		rec.NewChunks++
		rec.UploadedBytes += c.GetSize()
	}
	manifest, err := l.store.PresignPut(ctx, manifestKey(box, rec.ID))
	if err != nil {
		return nil, err
	}
	req.Manifest = &agentv1.PresignedPut{ChunkId: rec.ID, Url: manifest.URL, Headers: manifest.Headers}
	final, err := l.agents.Upload(ctx, addr, req, progress)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	if id := final.GetManifestId(); id != "" && id != rec.ID {
		return nil, fmt.Errorf("agent finished manifest %s, want %s", id, rec.ID)
	}
	rec.SavedAtMs = time.Now().UnixMilli()
	if err := l.commit(ctx, box, rec, stored); err != nil {
		return nil, err
	}
	return rec, nil
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
func (l *stateLayer) commit(ctx context.Context, box string, rec *snapshotRecord, stored map[string]bool) error {
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
	if err := putJSON(ctx, l.store, indexKey(box), ids); err != nil {
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

func (l *stateLayer) index(ctx context.Context, box string) (map[string]bool, error) {
	var ids []string
	if err := getJSON(ctx, l.store, indexKey(box), &ids); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	stored := make(map[string]bool, len(ids))
	for _, id := range ids {
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
