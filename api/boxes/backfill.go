package boxes

import (
	"bytes"
	"context"
	"strings"

	"github.com/loomhand/cocoonbox/api/store"
)

// BackfillHeads makes user's ledger heads match the store's head.json files and returns how many rows it wrote and removed.
func BackfillHeads(ctx context.Context, st store.Store, l Ledger, user string) (int, int, error) {
	p := statePath{user: user}
	listing, err := st.List(ctx, p.boxes())
	if err != nil {
		return 0, 0, err
	}
	rows, err := l.Heads(ctx)
	if err != nil {
		return 0, 0, err
	}
	stored := map[string][]byte{}
	for _, r := range rows {
		if r.User == user {
			stored[r.Box] = r.Head
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
		if row, ok := stored[box]; !ok || !bytes.Equal(row, raw) {
			if err := l.PutHead(ctx, user, box, raw); err != nil {
				return written, 0, err
			}
			written++
		}
		delete(stored, box)
	}
	for box := range stored {
		if err := l.DeleteHeads(ctx, user, box); err != nil {
			return written, 0, err
		}
	}
	return written, len(stored), nil
}
