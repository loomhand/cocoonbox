package boxes

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/loomhand/cocoonbox/api/cluster"
)

const (
	headKey = "head:"
	opKey   = "op:"
)

// Verify rewrites the shared operations that differ from this process's, returning their ids, and purges operations past their retention.
func (s *Service) Verify(ctx context.Context) ([]string, error) {
	differ, err := s.mirror.Verify(ctx, s.operationDifferences)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(differ))
	for _, key := range differ {
		id := strings.TrimPrefix(key, opKey)
		ids = append(ids, id)
		if op := s.ops.local(id); op != nil {
			op.mu.Lock()
			op.mirrorLocked()
			op.mu.Unlock()
		}
	}
	return ids, s.ledger.PurgeOperations(ctx, time.Now().Add(-operationRetention))
}

func (s *Service) operationDifferences(ctx context.Context) ([]string, error) {
	since := time.Now().Add(-operationRetention)
	rows, err := s.ledger.Operations(ctx, "", since)
	if err != nil {
		return nil, err
	}
	have := make(map[string]cluster.Operation, len(rows))
	for _, r := range rows {
		have[r.ID] = r
	}
	var differ []string
	for _, op := range s.ops.all() {
		op.mu.Lock()
		want := op.recordLocked()
		op.mu.Unlock()
		if !want.FinishedAt.IsZero() && !want.FinishedAt.After(since) {
			continue
		}
		if got, ok := have[want.ID]; !ok || !sameOperation(want, got) {
			differ = append(differ, opKey+want.ID)
		}
	}
	slices.Sort(differ)
	return differ, nil
}

func sameOperation(a, b cluster.Operation) bool {
	return a.User == b.User && a.Box == b.Box && a.Phase == b.Phase && a.Park == b.Park && a.Gen == b.Gen &&
		a.FinishedAt.IsZero() == b.FinishedAt.IsZero() && bytes.Equal(a.Events, b.Events)
}

func headBytes(h head) []byte {
	raw, _ := json.Marshal(h)
	return raw
}

func decodeHead(row cluster.Head) (head, error) {
	var h head
	if err := json.Unmarshal(row.Head, &h); err != nil {
		return head{}, fmt.Errorf("decode the shared head of %s: %w", row.Box, err)
	}
	h.version = row.Version
	return h, nil
}
