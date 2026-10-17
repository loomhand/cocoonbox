package boxes

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/loomhand/cocoonbox/api/store"
)

const purgeBatch = 1000

// deletedBox records when a GC pass first found a box's state without its box.
type deletedBox struct {
	DeletedAtMs int64 `json:"deleted_at_ms"`
}

// DeleteState removes every object of user's state from st and returns how many it removed; a user without state has none.
func DeleteState(ctx context.Context, st store.Store, user string) (int, error) {
	listing, err := st.List(ctx, user+"/")
	if err != nil {
		return 0, err
	}
	keys := make([]string, len(listing))
	for i, obj := range listing {
		keys[i] = obj.Key
	}
	for part := range slices.Chunk(keys, purgeBatch) {
		if err := st.Delete(ctx, part); err != nil {
			return 0, err
		}
	}
	return len(keys), nil
}

// DeleteState removes user's saved state once its machines are released; a user without state is not an error.
func (s *Service) DeleteState(ctx context.Context, user string) error {
	if s.state == nil {
		return nil
	}
	exclusive := s.state.userLock(user)
	exclusive.Lock()
	defer exclusive.Unlock()
	if _, err := DeleteState(ctx, s.state.store, user); err != nil {
		return err
	}
	s.state.forget(statePath{user: user})
	return nil
}

// expire removes the state of p's deleted box once opts.DeletedRetention has passed since the first pass that found it deleted, and reports whether it did.
func (l *stateLayer) expire(ctx context.Context, p statePath, opts GCOptions, now time.Time, r *gcReport) (bool, error) {
	r.deleted++
	var d deletedBox
	err := getJSON(ctx, l.store, p.deleted(), &d)
	if errors.Is(err, store.ErrNotFound) {
		d.DeletedAtMs, err = now.UnixMilli(), nil
		if !opts.DryRun {
			err = l.retry(ctx, func() error { return putJSON(ctx, l.store, p.deleted(), d) })
		}
	}
	if err != nil {
		return false, err
	}
	if now.Sub(time.UnixMilli(d.DeletedAtMs)) < opts.DeletedRetention {
		return false, nil
	}
	return true, l.purge(ctx, p, opts, r)
}

// purge deletes every object of p's box under its lock, the deletion record last.
func (l *stateLayer) purge(ctx context.Context, p statePath, opts GCOptions, r *gcReport) error {
	unlock := l.lock(p.box)
	defer unlock()
	listing, err := l.store.List(ctx, p.boxDir())
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(listing)+1)
	for _, obj := range listing {
		r.purgedBytes += obj.Size
		if obj.Key != p.deleted() {
			keys = append(keys, obj.Key)
		}
	}
	r.purged++
	if opts.DryRun {
		return nil
	}
	if err := l.deleteKeys(ctx, append(keys, p.deleted()), opts.DeleteBatch); err != nil {
		return err
	}
	l.forget(p)
	return nil
}

// forget drops the cached state of p's box, or of every box of p's user when p names none, and its ledger head.
func (l *stateLayer) forget(p statePath) {
	l.forgetCache(p)
	if l.mirror != nil {
		l.mirror(p, nil)
	}
}

// forgetCache drops the cached state of p's box, or of every box of p's user when p names none.
func (l *stateLayer) forgetCache(p statePath) {
	match := func(k statePath) bool { return k.user == p.user && (p.box == "" || k.box == p.box) }
	l.mu.Lock()
	defer l.mu.Unlock()
	maps.DeleteFunc(l.heads, func(k statePath, _ head) bool { return match(k) })
	maps.DeleteFunc(l.versions, func(k statePath, _ int64) bool { return match(k) })
	maps.DeleteFunc(l.browsed, func(k statePath, _ browsed) bool { return match(k) })
	maps.DeleteFunc(l.bases, func(k statePath, _ *snapshotRecord) bool { return match(k) })
	l.dropped++
}
