package boxes

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/store"
)

// BackfillHeads makes user's ledger heads match the store's head.json files, never over a newer or concurrently written row, and returns how many rows it wrote and removed.
func BackfillHeads(ctx context.Context, st store.Store, l *cluster.Store, user string) (int, int, error) {
	p := statePath{user: user}
	listing, err := st.List(ctx, p.boxes())
	if err != nil {
		return 0, 0, err
	}
	rows, err := l.Heads(ctx)
	if err != nil {
		return 0, 0, err
	}
	stored := map[string]cluster.Head{}
	for _, r := range rows {
		if r.User == user {
			stored[r.Box] = r
		}
	}
	written := 0
	for _, obj := range listing {
		box, ok := strings.CutSuffix(strings.TrimPrefix(obj.Key, p.boxes()), "/head.json")
		if !ok || strings.Contains(box, "/") {
			continue
		}
		var h head
		if err := getJSON(ctx, st, obj.Key, &h); err != nil {
			return written, 0, err
		}
		raw := headBytes(h)
		row, ok := stored[box]
		delete(stored, box)
		if ok {
			cur, err := decodeHead(row)
			if err != nil {
				return written, 0, err
			}
			if bytes.Equal(row.Head, raw) || cur.SavedAtMs > h.SavedAtMs {
				continue
			}
		}
		switch _, err := l.SwapHead(ctx, user, box, raw, row.Version); {
		case errors.Is(err, cluster.ErrStale):
		case err != nil:
			return written, 0, err
		default:
			written++
		}
	}
	removed := 0
	for box, row := range stored {
		gone, err := l.DeleteHeadAt(ctx, user, box, row.Version)
		if err != nil {
			return written, removed, err
		}
		if gone {
			removed++
		}
	}
	return written, removed, nil
}
