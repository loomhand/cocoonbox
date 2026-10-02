package boxes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/CMGS/cocoonbox/api/store"
)

const migrateCopies = 16

// LayoutStore is a store that lists and copies objects inside itself.
type LayoutStore interface {
	store.Store
	List(ctx context.Context, prefix string) ([]string, error)
	Copy(ctx context.Context, src, dst string) error
}

// MigrateState copies user's box from the per-box layout into the per-user one and returns the objects copied; a rerun copies only what is missing and never moves the head back.
func MigrateState(ctx context.Context, st LayoutStore, user string) (int, error) {
	p := statePath{user: user, box: BoxID(user)}
	old := p.box + "/"
	var oldHead, newHead head
	var oldIndex, newIndex chunkIndex
	for key, v := range map[string]any{old + "head.json": &oldHead, old + "index.json": &oldIndex, p.head(): &newHead, p.index(): &newIndex} {
		if err := getJSON(ctx, st, key, v); err != nil && !errors.Is(err, store.ErrNotFound) {
			return 0, err
		}
	}
	objects, err := st.List(ctx, old)
	if err != nil {
		return 0, err
	}
	present, err := st.List(ctx, user+"/")
	if err != nil {
		return 0, err
	}
	have := make(map[string]bool, len(present))
	for _, key := range present {
		have[key] = true
	}
	var copied atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(migrateCopies)
	for _, src := range objects {
		dst, err := migratedKey(p, strings.TrimPrefix(src, old))
		if err != nil {
			return 0, errors.Join(err, g.Wait())
		}
		if dst == "" || have[dst] {
			continue
		}
		g.Go(func() error {
			if err := st.Copy(gctx, src, dst); err != nil {
				return err
			}
			copied.Add(1)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return int(copied.Load()), err
	}
	if err := mergeIndex(ctx, st, p, oldIndex, newIndex); err != nil {
		return int(copied.Load()), err
	}
	if oldHead.SavedAtMs > newHead.SavedAtMs {
		if err := putJSON(ctx, st, p.head(), oldHead); err != nil {
			return int(copied.Load()), err
		}
	}
	return int(copied.Load()), nil
}

// mergeIndex writes the union of both indexes; their chunks are in the new layout once the copies finish.
func mergeIndex(ctx context.Context, st store.Store, p statePath, old, cur chunkIndex) error {
	if old.KeyID == "" {
		return nil
	}
	if cur.KeyID != "" && cur.KeyID != old.KeyID {
		return fmt.Errorf("index of %s is under key %s, the box's under %s", p.user, cur.KeyID, old.KeyID)
	}
	merged := slices.Compact(slices.Sorted(slices.Values(slices.Concat(old.Chunks, cur.Chunks))))
	if cur.KeyID == old.KeyID && len(merged) == len(cur.Chunks) {
		return nil
	}
	return putJSON(ctx, st, p.index(), chunkIndex{KeyID: old.KeyID, Chunks: merged})
}

// migratedKey maps an object of the per-box layout to its per-user key; the index and head are merged, not copied.
func migratedKey(p statePath, rel string) (string, error) {
	dir, name, _ := strings.Cut(rel, "/")
	switch {
	case rel == "index.json" || rel == "head.json":
		return "", nil
	case name == "" || strings.Contains(name, "/"):
	case dir == "chunks":
		return p.chunk(name), nil
	case dir == "manifests":
		return p.manifest(name), nil
	case dir == "snapshots":
		return p.boxDir() + rel, nil
	}
	return "", fmt.Errorf("unexpected object %s/%s", p.box, rel)
}
