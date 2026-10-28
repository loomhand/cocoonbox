package boxes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/store"
)

const (
	manifestDeltaVersion = 2
	// deltaShare bounds a delta record to 1/deltaShare of its base's chunks; a larger change is saved full.
	deltaShare  = 4
	deltaSuffix = ".d"
)

var errNotSplittable = errors.New("manifest is not compact JSON with one files array")

// recordFile is a snapshot record as stored: Chunks is an array in a full record and a chunkDelta against Base in a delta record, which an api without delta records fails to decode.
type recordFile struct {
	timelineEntry
	Parent string          `json:"parent,omitempty"`
	Chunks json.RawMessage `json:"chunks"`
}

// chunkDelta lists what a delta record holds beyond its base and what of its base it no longer holds.
type chunkDelta struct {
	Added   []chunk  `json:"added"`
	Removed []string `json:"removed"`
}

// manifestDelta rebuilds a manifest's exact plaintext from its base: Prefix, the files array from Ops, then Suffix.
type manifestDelta struct {
	Version int      `json:"version"`
	Base    string   `json:"base"`
	Prefix  string   `json:"prefix"`
	Suffix  string   `json:"suffix"`
	Ops     []fileOp `json:"ops"`
}

// apply rebuilds the files of the manifest d describes from base's.
func (d manifestDelta) apply(base manifestParts) (manifestParts, error) {
	out := manifestParts{prefix: []byte(d.Prefix), suffix: []byte(d.Suffix)}
	for _, op := range d.Ops {
		switch {
		case op.Count == 0:
			out.files = append(out.files, json.RawMessage(op.Put))
		case op.From < 0 || op.Count < 0 || op.From+op.Count > len(base.files):
			return out, fmt.Errorf("delta copies files %d+%d of a base with %d", op.From, op.Count, len(base.files))
		default:
			out.files = append(out.files, base.files[op.From:op.From+op.Count]...)
		}
	}
	return out, nil
}

// fileOp copies Count files of the base from index From, or, with Count 0, puts one file record verbatim.
type fileOp struct {
	From  int    `json:"from,omitzero"`
	Count int    `json:"count,omitzero"`
	Put   string `json:"put,omitempty"`
}

// manifestParts is a compact manifest split around its files array, each file record verbatim.
type manifestParts struct {
	prefix []byte
	files  []json.RawMessage
	suffix []byte
}

func (m manifestParts) join() []byte {
	var b bytes.Buffer
	b.Write(m.prefix)
	b.WriteByte('[')
	for i, f := range m.files {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(f)
	}
	b.WriteByte(']')
	b.Write(m.suffix)
	return b.Bytes()
}

// deltaBase returns the record new saves of p's box are written against, kept in memory per box: the newest full snapshot saved on day's UTC date, nil when there is none or p's user writes full records.
func (l *stateLayer) deltaBase(ctx context.Context, p statePath, day time.Time) (*snapshotRecord, error) {
	if l.deltaFor == nil || !l.deltaFor(p.user) {
		return nil, nil
	}
	tl, found, err := l.readTimeline(ctx, p)
	if err != nil || !found {
		return nil, err
	}
	date := day.UTC().Format(time.DateOnly)
	for _, e := range slices.Backward(tl.Snapshots) {
		if e.Base != "" || time.UnixMilli(e.SavedAtMs).UTC().Format(time.DateOnly) != date {
			continue
		}
		l.mu.Lock()
		cached := l.bases[p]
		l.mu.Unlock()
		if cached != nil && cached.ID == e.ID {
			return cached, nil
		}
		base, err := l.record(ctx, p, e.ID)
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		l.bases[p] = base
		l.mu.Unlock()
		return base, nil
	}
	return nil, nil
}

// putRecord writes rec as a delta against base when base is set and the change stays within 1/deltaShare of it, else full; it sets rec.Base to match.
func (l *stateLayer) putRecord(ctx context.Context, p statePath, rec *snapshotRecord, base *snapshotRecord) error {
	rec.Base = ""
	body, err := json.Marshal(rec.Chunks)
	if err != nil {
		return err
	}
	if base != nil {
		if d, ok := chunkDiff(base.Chunks, rec.Chunks); ok {
			rec.Base = base.ID
			if body, err = json.Marshal(d); err != nil {
				return err
			}
		}
	}
	return putJSON(ctx, l.store, p.record(rec.ID), recordFile{timelineEntry: rec.timelineEntry, Parent: rec.Parent, Chunks: body})
}

// readRecord reads snapshot id of p's box and rebuilds a delta record from its base, taking bases from and adding them to bases when it is not nil.
func (l *stateLayer) readRecord(ctx context.Context, p statePath, id string, bases map[string]*snapshotRecord) (*snapshotRecord, error) {
	var file recordFile
	if err := getJSON(ctx, l.store, p.record(id), &file); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", id, err)
	}
	rec := &snapshotRecord{timelineEntry: file.timelineEntry, Parent: file.Parent}
	if file.Base == "" {
		if err := json.Unmarshal(file.Chunks, &rec.Chunks); err != nil {
			return nil, fmt.Errorf("decode snapshot %s: %w", id, err)
		}
		return rec, nil
	}
	var d chunkDelta
	if err := json.Unmarshal(file.Chunks, &d); err != nil {
		return nil, fmt.Errorf("decode delta snapshot %s: %w", id, err)
	}
	base := bases[file.Base]
	if base == nil {
		var err error
		if base, err = l.readRecord(ctx, p, file.Base, nil); err != nil {
			return nil, fmt.Errorf("base of snapshot %s: %w", id, err)
		}
		if base.Base != "" {
			return nil, fmt.Errorf("base %s of snapshot %s is itself a delta", file.Base, id)
		}
		if bases != nil {
			bases[file.Base] = base
		}
	}
	removed := map[string]bool{}
	for _, c := range d.Removed {
		removed[c] = true
	}
	for _, c := range base.Chunks {
		if !removed[c.ID] {
			rec.Chunks = append(rec.Chunks, c)
		}
	}
	rec.Chunks = append(rec.Chunks, d.Added...)
	return rec, nil
}

// manifestPlain returns the plaintext of snapshot id's manifest, rebuilt from its base when only a delta is stored, and checks it against id.
func (l *stateLayer) manifestPlain(ctx context.Context, p statePath, key keyring.Key, id string) ([]byte, error) {
	sealed, err := l.store.Get(ctx, p.manifest(id))
	if err == nil {
		return openSealed(key.Secret, id, sealed, manifestLimit)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("manifest %s: %w", id, err)
	}
	raw, err := l.store.Get(ctx, p.manifest(id)+deltaSuffix)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", id, err)
	}
	plain, err := openObject(key.Secret, id+deltaSuffix, raw, manifestLimit)
	if err != nil {
		return nil, err
	}
	var d manifestDelta
	if err = json.Unmarshal(plain, &d); err != nil {
		return nil, fmt.Errorf("decode manifest delta %s: %w", id, err)
	}
	if d.Version != manifestDeltaVersion || !objectID.MatchString(d.Base) {
		return nil, fmt.Errorf("manifest delta %s has version %d and base %q", id, d.Version, d.Base)
	}
	baseSealed, err := l.store.Get(ctx, p.manifest(d.Base))
	if err != nil {
		return nil, fmt.Errorf("base manifest %s of %s: %w", d.Base, id, err)
	}
	basePlain, err := openSealed(key.Secret, d.Base, baseSealed, manifestLimit)
	if err != nil {
		return nil, err
	}
	base, err := splitManifest(basePlain)
	if err != nil {
		return nil, err
	}
	parts, err := d.apply(base)
	if err != nil {
		return nil, err
	}
	full := parts.join()
	if !matchesID(key.Secret, id, full) {
		return nil, fmt.Errorf("manifest %s rebuilt from %s does not match its id", id, d.Base)
	}
	return full, nil
}

// materialize writes snapshot id's full manifest back when only its delta is stored, so an agent can read it; the GC compacts it again later.
func (l *stateLayer) materialize(ctx context.Context, p statePath, key keyring.Key, id string) error {
	if _, err := l.store.Get(ctx, p.manifest(id)+deltaSuffix); errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	plain, err := l.manifestPlain(ctx, p, key, id)
	if err != nil {
		return err
	}
	sealed, err := sealObject(key.Secret, id, plain)
	if err != nil {
		return err
	}
	return l.store.Put(ctx, p.manifest(id), sealed)
}

// compactManifest replaces snapshot e's full manifest by a delta against its base's, after checking that the delta rebuilds it exactly.
func (l *stateLayer) compactManifest(ctx context.Context, p statePath, key keyring.Key, e timelineEntry, bases map[string]manifestParts) error {
	base, ok := bases[e.Base]
	if !ok {
		plain, err := l.manifestPlain(ctx, p, key, e.Base)
		if err != nil {
			return err
		}
		if base, err = splitManifest(plain); err != nil {
			return err
		}
		bases[e.Base] = base
	}
	plain, err := l.manifestPlain(ctx, p, key, e.ID)
	if err != nil {
		return err
	}
	next, err := splitManifest(plain)
	if err != nil {
		return err
	}
	d := diffManifest(base, next, e.Base)
	if check, aerr := d.apply(base); aerr != nil || !bytes.Equal(check.join(), plain) {
		return fmt.Errorf("delta of manifest %s does not rebuild it", e.ID)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	sealed, err := sealObject(key.Secret, e.ID+deltaSuffix, raw)
	if err != nil {
		return err
	}
	if err := l.store.Put(ctx, p.manifest(e.ID)+deltaSuffix, sealed); err != nil {
		return err
	}
	return l.store.Delete(ctx, []string{p.manifest(e.ID)})
}

// expandSnapshot rewrites snapshot id of p's box as a full record with a full manifest, so it no longer needs its base.
func (l *stateLayer) expandSnapshot(ctx context.Context, p statePath, key keyring.Key, id string) error {
	rec, err := l.record(ctx, p, id)
	if err != nil {
		return err
	}
	if err := l.materialize(ctx, p, key, id); err != nil {
		return err
	}
	if err := l.putRecord(ctx, p, rec, nil); err != nil {
		return err
	}
	return l.store.Delete(ctx, []string{p.manifest(id) + deltaSuffix})
}

// deltaIDs returns the snapshots of p's box stored as a delta record or a manifest delta, sorted.
func (l *stateLayer) deltaIDs(ctx context.Context, p statePath) ([]string, error) {
	ids := map[string]bool{}
	records, err := l.recordIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, id := range records {
		var e timelineEntry
		if err = getJSON(ctx, l.store, p.record(id), &e); err != nil {
			return nil, err
		}
		if e.Base != "" {
			ids[id] = true
		}
	}
	listing, err := l.store.List(ctx, p.manifests())
	if err != nil {
		return nil, err
	}
	for _, obj := range listing {
		if id, ok := strings.CutSuffix(strings.TrimPrefix(obj.Key, p.manifests()), deltaSuffix); ok && objectID.MatchString(id) {
			ids[id] = true
		}
	}
	return slices.Sorted(maps.Keys(ids)), nil
}

// ExpandState rewrites every delta record and manifest of user's boxes as full objects, the layout an api before delta records reads; run it with the api stopped.
func ExpandState(ctx context.Context, st store.Store, keys keyring.Keyring, user string) (int, error) {
	l := newStateLayer(nil, st, keys)
	key := sync.OnceValues(func() (keyring.Key, error) { return keys.StateKey(ctx, user) })
	boxes, err := l.boxesOf(ctx, user)
	if err != nil {
		return 0, err
	}
	expanded := 0
	for _, box := range boxes {
		p := statePath{user: user, box: box}
		ids, err := l.deltaIDs(ctx, p)
		if err != nil {
			return expanded, err
		}
		for _, id := range ids {
			k, kerr := key()
			if kerr != nil {
				return expanded, fmt.Errorf("state key: %w", kerr)
			}
			if err = l.expandSnapshot(ctx, p, k, id); err != nil {
				return expanded, fmt.Errorf("expand %s of box %s: %w", id, box, err)
			}
			expanded++
		}
		tl, found, err := l.readTimeline(ctx, p)
		if err != nil {
			return expanded, err
		}
		if !found {
			continue
		}
		for i := range tl.Snapshots {
			tl.Snapshots[i].Base = ""
		}
		if err := putJSON(ctx, st, p.timeline(), tl); err != nil {
			return expanded, err
		}
	}
	return expanded, nil
}

// chunkDiff returns next as a delta against base, or false when the delta would list more than 1/deltaShare of base's chunks.
func chunkDiff(base, next []chunk) (chunkDelta, bool) {
	inBase, inNext := map[string]bool{}, map[string]bool{}
	for _, c := range base {
		inBase[c.ID] = true
	}
	d := chunkDelta{Added: []chunk{}, Removed: []string{}}
	for _, c := range next {
		if !inBase[c.ID] && !inNext[c.ID] {
			d.Added = append(d.Added, c)
		}
		inNext[c.ID] = true
	}
	for _, c := range base {
		if !inNext[c.ID] {
			d.Removed = append(d.Removed, c.ID)
		}
	}
	return d, (len(d.Added)+len(d.Removed))*deltaShare <= len(base)
}

// splitManifest splits a compact manifest around its files array and refuses one that does not join back to the same bytes.
func splitManifest(plain []byte) (manifestParts, error) {
	i := bytes.Index(plain, []byte(`"files":[`))
	if i < 0 {
		return manifestParts{}, errNotSplittable
	}
	start := i + len(`"files":`)
	dec := json.NewDecoder(bytes.NewReader(plain[start:]))
	var files []json.RawMessage
	if err := dec.Decode(&files); err != nil {
		return manifestParts{}, fmt.Errorf("%w: %w", errNotSplittable, err)
	}
	parts := manifestParts{prefix: plain[:start], files: files, suffix: plain[start+int(dec.InputOffset()):]}
	if !bytes.Equal(parts.join(), plain) {
		return manifestParts{}, errNotSplittable
	}
	return parts, nil
}

// diffManifest describes next as runs of base's file records and the records base lacks.
func diffManifest(base, next manifestParts, baseID string) manifestDelta {
	at := make(map[string]int, len(base.files))
	for i, f := range base.files {
		at[string(f)] = i
	}
	d := manifestDelta{Version: manifestDeltaVersion, Base: baseID, Prefix: string(next.prefix), Suffix: string(next.suffix)}
	for _, f := range next.files {
		i, ok := at[string(f)]
		last := len(d.Ops) - 1
		switch {
		case !ok:
			d.Ops = append(d.Ops, fileOp{Put: string(f)})
		case last >= 0 && d.Ops[last].Count > 0 && d.Ops[last].From+d.Ops[last].Count == i:
			d.Ops[last].Count++
		default:
			d.Ops = append(d.Ops, fileOp{From: i, Count: 1})
		}
	}
	return d
}
