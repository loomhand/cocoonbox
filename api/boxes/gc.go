package boxes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/store"
)

const (
	retainAll   = 24 * time.Hour
	retainDaily = 30 * 24 * time.Hour
)

// GCOptions configures the retention and chunk collection passes.
type GCOptions struct {
	Interval time.Duration
	DryRun   bool
	// Users limits the passes to these user ids and those Flagged returns; an empty Users means every user.
	Users       []string
	Flagged     func() []string
	DeleteBatch int
	// DeletedRetention keeps a deleted box's state this long after the first pass that finds its box gone.
	DeletedRetention time.Duration
}

// gcReport counts what one pass removed for one user, or would remove in a dry run.
type gcReport struct {
	boxes         int
	kept          int
	pruned        int
	recordBytes   int64
	manifestBytes int64
	stored        int
	chunks        int
	chunkBytes    int64
	indexEntries  int
	expanded      int
	compacted     int
	deleted       int
	purged        int
	purgedBytes   int64
}

// keySource returns the user's state key, read once per pass.
type keySource func() (keyring.Key, error)

// RunGC prunes and collects the state of every user that users returns, at once and then every opts.Interval, until ctx ends.
func (s *Service) RunGC(ctx context.Context, opts GCOptions, users func() []string) {
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		s.collectGarbage(ctx, opts, users(), time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) collectGarbage(ctx context.Context, opts GCOptions, users []string, now time.Time) {
	logger := log.WithFunc("boxes.Service.collectGarbage")
	verb := "removed"
	if opts.DryRun {
		verb = "dry run, would remove"
	}
	allowed := opts.Users
	if len(allowed) > 0 && opts.Flagged != nil {
		allowed = slices.Concat(allowed, opts.Flagged())
	}
	for _, user := range users {
		if len(allowed) > 0 && !slices.Contains(allowed, user) {
			continue
		}
		start := time.Now()
		r, err := s.state.collect(ctx, user, opts, now, func(box string) bool { return s.registry.HasBox(user, box) })
		if err != nil {
			logger.Errorf(ctx, err, "collect the state of user %s", user)
			continue
		}
		logger.Infof(ctx, "user %s: %s %d snapshots (%d kept in %d boxes) and %d of %d chunks (%d index entries), %.1f MiB: records %.1f, manifests %.1f, chunks %.1f; %d of %d deleted boxes purged, %.1f MiB; %d made full, %d manifests compacted; %s",
			user, verb, r.pruned, r.kept, r.boxes, r.chunks, r.stored, r.indexEntries,
			float64(r.recordBytes+r.manifestBytes+r.chunkBytes)/mib, float64(r.recordBytes)/mib, float64(r.manifestBytes)/mib, float64(r.chunkBytes)/mib,
			r.purged, r.deleted, float64(r.purgedBytes)/mib, r.expanded, r.compacted, time.Since(start).Round(time.Millisecond))
	}
}

// collect prunes every box of user and purges the expired state of the boxes live denies, asked after the listing since a box saves only while live and no box id is reused, then deletes the chunks that no kept snapshot references.
func (l *stateLayer) collect(ctx context.Context, user string, opts GCOptions, now time.Time, live func(box string) bool) (gcReport, error) {
	var r gcReport
	boxes, err := l.boxesOf(ctx, user)
	if err != nil {
		return r, err
	}
	kept, dropped, purged := map[string][]string{}, map[string]map[string]bool{}, map[string]bool{}
	for _, box := range boxes {
		p := statePath{user: user, box: box}
		if !live(box) {
			if purged[box], err = l.expire(ctx, p, opts, now, &r); err != nil {
				return r, fmt.Errorf("purge box %s: %w", box, err)
			}
			if purged[box] {
				continue
			}
		}
		if kept[box], dropped[box], err = l.prune(ctx, p, opts, now, &r); err != nil {
			return r, fmt.Errorf("prune box %s: %w", box, err)
		}
	}
	r.boxes = len(kept)
	return r, l.sweep(ctx, user, opts, kept, dropped, purged, &r)
}

// prune applies the retention rule to p's timeline under the box lock, makes the only dependent of a dropped base full, removes the dropped snapshots and every record or manifest the timeline does not name or need, and compacts the kept manifests.
func (l *stateLayer) prune(ctx context.Context, p statePath, opts GCOptions, now time.Time, r *gcReport) ([]string, map[string]bool, error) {
	tl, found, err := l.readTimeline(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	var built map[string]timelineEntry
	if !found {
		// Records are immutable, so the long read of all of them runs before the box lock.
		if built, err = l.readEntries(ctx, p, nil); err != nil {
			return nil, nil, err
		}
	}
	unlock := l.lock(p.box)
	defer unlock()
	var version string
	if found {
		tl = timeline{}
		if version, _, err = l.readVersion(ctx, p.timeline(), &tl); err != nil {
			return nil, nil, err
		}
	} else {
		if built, err = l.readEntries(ctx, p, built); err != nil {
			return nil, nil, err
		}
		tl.Snapshots = slices.SortedFunc(maps.Values(built), compareEntries)
	}
	h, err := l.head(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	listing, err := l.store.List(ctx, p.boxDir())
	if err != nil {
		return nil, nil, err
	}
	keep := l.pinned(p)
	if h.Latest != "" {
		keep[h.Latest] = true
	}
	retained := retain(tl.Snapshots, now, keep)
	for id := range keep {
		if !retained[id] {
			return nil, nil, fmt.Errorf("snapshot %s is the head or pinned but not in the timeline", id)
		}
	}
	dropped := map[string]bool{}
	var next timeline
	for _, e := range tl.Snapshots {
		if retained[e.ID] {
			next.Snapshots = append(next.Snapshots, e)
		} else {
			dropped[e.ID] = true
		}
	}
	keys := sync.OnceValues(func() (keyring.Key, error) { return l.keys.StateKey(ctx, p.user) })
	needed, rebased, err := l.settleBases(ctx, p, &next, retained, keys, opts.DryRun, r)
	if err != nil {
		return nil, nil, err
	}
	records, manifests, full := obsolete(p, listing, func(id string) bool { return retained[id] || needed[id] }, dropped, r)
	r.kept += len(next.Snapshots)
	r.pruned += len(dropped)
	ids := make([]string, 0, len(next.Snapshots))
	for _, e := range next.Snapshots {
		ids = append(ids, e.ID)
	}
	if opts.DryRun {
		return ids, dropped, nil
	}
	if !found || rebased || len(next.Snapshots) != len(tl.Snapshots) {
		if err := l.retry(ctx, func() error { return l.writeVersion(ctx, p.timeline(), next, version) }); err != nil {
			return nil, nil, err
		}
	}
	if err := l.deleteKeys(ctx, records, opts.DeleteBatch); err != nil {
		return nil, nil, err
	}
	if err := l.deleteKeys(ctx, manifests, opts.DeleteBatch); err != nil {
		return nil, nil, err
	}
	l.compact(ctx, p, next, keep, full, keys, r)
	return ids, dropped, nil
}

// settleBases makes the only kept dependent of a dropped base full, so the base can go, and returns the dropped bases that several kept snapshots still need.
func (l *stateLayer) settleBases(ctx context.Context, p statePath, next *timeline, retained map[string]bool, keys keySource, dryRun bool, r *gcReport) (map[string]bool, bool, error) {
	dependents := map[string][]int{}
	for i, e := range next.Snapshots {
		if e.Base != "" && !retained[e.Base] {
			dependents[e.Base] = append(dependents[e.Base], i)
		}
	}
	needed, rebased := map[string]bool{}, false
	for base, idx := range dependents {
		if len(idx) > 1 {
			needed[base] = true
			continue
		}
		r.expanded++
		if dryRun {
			continue
		}
		key, err := keys()
		if err != nil {
			return nil, false, err
		}
		if err := l.expandSnapshot(ctx, p, key, next.Snapshots[idx[0]].ID); err != nil {
			return nil, false, fmt.Errorf("expand snapshot %s off base %s: %w", next.Snapshots[idx[0]].ID, base, err)
		}
		next.Snapshots[idx[0]].Base = ""
		rebased = true
	}
	return needed, rebased, nil
}

// compact turns the full manifest of every kept delta snapshot other than the head and the pinned ones into a delta; a failure leaves that manifest full.
func (l *stateLayer) compact(ctx context.Context, p statePath, next timeline, keep, full map[string]bool, keys keySource, r *gcReport) {
	logger := log.WithFunc("boxes.stateLayer.compact").WithField("box", p.box)
	bases := map[string]manifestParts{}
	for _, e := range next.Snapshots {
		if e.Base == "" || keep[e.ID] || !full[e.ID] {
			continue
		}
		key, err := keys()
		if err == nil {
			err = l.compactManifest(ctx, p, key, e, bases)
		}
		if err != nil {
			logger.Warnf(ctx, "keep the full manifest of %s: %v", e.ID, err)
			continue
		}
		r.compacted++
	}
}

// sweep marks the chunks of kept without a lock, then under the user lock marks the snapshots committed since outside purged boxes, writes the index without the garbage and deletes it.
func (l *stateLayer) sweep(ctx context.Context, user string, opts GCOptions, kept map[string][]string, dropped map[string]map[string]bool, purged map[string]bool, r *gcReport) error {
	p := statePath{user: user}
	marked, seen := map[string]bool{}, map[string]map[string]bool{}
	for box, ids := range kept {
		if err := l.mark(ctx, statePath{user: user, box: box}, ids, marked, seen); err != nil {
			return err
		}
	}
	listing, err := l.store.List(ctx, p.chunks())
	if err != nil {
		return err
	}
	exclusive := l.userLock(user)
	exclusive.Lock()
	defer exclusive.Unlock()
	unlock := l.lock(p.index())
	defer unlock()
	var idx chunkIndex
	version, _, err := l.readVersion(ctx, p.index(), &idx)
	if err != nil {
		return err
	}
	boxes, err := l.boxesOf(ctx, user)
	if err != nil {
		return err
	}
	for _, box := range boxes {
		if purged[box] {
			continue
		}
		bp := statePath{user: user, box: box}
		ids, err := l.snapshotIDs(ctx, bp)
		if err != nil {
			return err
		}
		fresh := slices.DeleteFunc(ids, func(id string) bool { return seen[box][id] || dropped[box][id] })
		if err := l.mark(ctx, bp, fresh, marked, seen); err != nil {
			return err
		}
	}
	garbage := map[string]bool{}
	indexed := slices.DeleteFunc(slices.Clone(idx.Chunks), func(id string) bool {
		if marked[id] {
			return false
		}
		garbage[p.chunk(id)] = true
		return true
	})
	r.indexEntries = len(idx.Chunks) - len(indexed)
	r.stored = len(listing)
	for _, obj := range listing {
		if id := strings.TrimPrefix(obj.Key, p.chunks()); objectID.MatchString(id) && !marked[id] {
			garbage[obj.Key] = true
			r.chunks++
			r.chunkBytes += obj.Size
		}
	}
	if opts.DryRun {
		return nil
	}
	// The index goes first: a save must never skip uploading a chunk that the deletes remove.
	if r.indexEntries > 0 {
		next := chunkIndex{KeyID: idx.KeyID, Chunks: indexed, Generation: idx.Generation + 1, Commits: idx.Commits}
		if err := l.retry(ctx, func() error { return l.writeVersion(ctx, p.index(), next, version) }); err != nil {
			return err
		}
	}
	return l.deleteKeys(ctx, slices.Sorted(maps.Keys(garbage)), opts.DeleteBatch)
}

// mark adds the chunks of p's snapshots ids to marked and records each id in seen.
func (l *stateLayer) mark(ctx context.Context, p statePath, ids []string, marked map[string]bool, seen map[string]map[string]bool) error {
	if seen[p.box] == nil {
		seen[p.box] = map[string]bool{}
	}
	bases := map[string]*snapshotRecord{}
	for _, id := range ids {
		rec, err := l.readRecord(ctx, p, id, bases)
		if err != nil {
			return err
		}
		for _, c := range rec.Chunks {
			marked[c.ID] = true
		}
		seen[p.box][id] = true
	}
	return nil
}

// boxesOf returns the boxes that hold snapshots of user.
func (l *stateLayer) boxesOf(ctx context.Context, user string) ([]string, error) {
	dir := statePath{user: user}.boxes()
	listing, err := l.store.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	boxes := map[string]bool{}
	for _, obj := range listing {
		if box, _, ok := strings.Cut(strings.TrimPrefix(obj.Key, dir), "/"); ok {
			boxes[box] = true
		}
	}
	return slices.Sorted(maps.Keys(boxes)), nil
}

// snapshotIDs returns the snapshots p's timeline names, or every listed record of a box without one.
func (l *stateLayer) snapshotIDs(ctx context.Context, p statePath) ([]string, error) {
	tl, found, err := l.readTimeline(ctx, p)
	if err != nil {
		return nil, err
	}
	if found {
		ids := make([]string, 0, len(tl.Snapshots))
		for _, e := range tl.Snapshots {
			ids = append(ids, e.ID)
		}
		return ids, nil
	}
	return l.recordIDs(ctx, p)
}

// readEntries adds the timeline entry of every record of p's box missing from have.
func (l *stateLayer) readEntries(ctx context.Context, p statePath, have map[string]timelineEntry) (map[string]timelineEntry, error) {
	ids, err := l.recordIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	if have == nil {
		have = map[string]timelineEntry{}
	}
	for _, id := range ids {
		if _, ok := have[id]; ok {
			continue
		}
		var e timelineEntry
		if err := getJSON(ctx, l.store, p.record(id), &e); err != nil {
			return nil, err
		}
		have[id] = e
	}
	return have, nil
}

func (l *stateLayer) recordIDs(ctx context.Context, p statePath) ([]string, error) {
	listing, err := l.store.List(ctx, p.records())
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, obj := range listing {
		if id, ok := strings.CutSuffix(strings.TrimPrefix(obj.Key, p.records()), ".json"); ok && objectID.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (l *stateLayer) readTimeline(ctx context.Context, p statePath) (timeline, bool, error) {
	var tl timeline
	err := getJSON(ctx, l.store, p.timeline(), &tl)
	if errors.Is(err, store.ErrNotFound) {
		return tl, false, nil
	}
	return tl, err == nil, err
}

func (l *stateLayer) deleteKeys(ctx context.Context, keys []string, batch int) error {
	for part := range slices.Chunk(keys, batch) {
		if err := l.store.Delete(ctx, part); err != nil {
			return err
		}
	}
	return nil
}

// obsolete sorts the records and manifests in listing that keep does not name into dropped and r, and returns them with the snapshots that have a full manifest.
func obsolete(p statePath, listing []store.Object, keep func(string) bool, dropped map[string]bool, r *gcReport) ([]string, []string, map[string]bool) {
	var records, manifests []string
	full := map[string]bool{}
	for _, obj := range listing {
		dir, name, _ := strings.Cut(strings.TrimPrefix(obj.Key, p.boxDir()), "/")
		id := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), deltaSuffix)
		manifest := dir == "manifests" && (name == id || name == id+deltaSuffix)
		if manifest && name == id {
			full[id] = true
		}
		if !objectID.MatchString(id) || keep(id) {
			continue
		}
		switch {
		case dir == "snapshots" && name == id+".json":
			records = append(records, obj.Key)
			r.recordBytes += obj.Size
		case manifest:
			manifests = append(manifests, obj.Key)
			r.manifestBytes += obj.Size
		default:
			continue
		}
		dropped[id] = true
	}
	return records, manifests, full
}

// retain returns the ids the rule keeps: all of the last day, the newest of each UTC day for 30 days, the newest of each UTC week, and keep.
func retain(entries []timelineEntry, now time.Time, keep map[string]bool) map[string]bool {
	newestDay, newestWeek := map[string]timelineEntry{}, map[string]timelineEntry{}
	for _, e := range entries {
		day, week := buckets(e)
		if cur, ok := newestDay[day]; !ok || compareEntries(e, cur) > 0 {
			newestDay[day] = e
		}
		if cur, ok := newestWeek[week]; !ok || compareEntries(e, cur) > 0 {
			newestWeek[week] = e
		}
	}
	kept := map[string]bool{}
	for _, e := range entries {
		day, week := buckets(e)
		age := now.Sub(time.UnixMilli(e.SavedAtMs))
		if keep[e.ID] || age < retainAll || (age < retainDaily && newestDay[day].ID == e.ID) || newestWeek[week].ID == e.ID {
			kept[e.ID] = true
		}
	}
	return kept
}

func buckets(e timelineEntry) (string, string) {
	t := time.UnixMilli(e.SavedAtMs).UTC()
	year, week := t.ISOWeek()
	return t.Format(time.DateOnly), fmt.Sprintf("%d-W%02d", year, week)
}

func compareEntries(a, b timelineEntry) int {
	return cmp.Or(cmp.Compare(a.SavedAtMs, b.SavedAtMs), strings.Compare(a.ID, b.ID))
}
