package boxes

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/loomhand/cocoonbox/api/cluster"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
)

// Ledger keeps the box heads and operations api processes share.
type Ledger interface {
	Heads(ctx context.Context) ([]cluster.Head, error)
	PutHead(ctx context.Context, user, box string, head []byte) (int64, error)
	DeleteHeads(ctx context.Context, user, box string) error
	Operations(ctx context.Context, since time.Time) ([]cluster.Operation, error)
	PutOperation(ctx context.Context, op cluster.Operation) error
	PurgeOperations(ctx context.Context, before time.Time) error
}

// Drift is what a Verify pass found the ledger missing or holding wrong.
type Drift struct {
	Heads      []string
	Operations []string
	// Backfilled counts the heads the ledger lacked, read before the ledger kept heads.
	Backfilled int
}

// eventRecord is an operation event as the ledger keeps it.
type eventRecord struct {
	Phase      int32  `json:"phase"`
	Detail     string `json:"detail,omitempty"`
	OffsetKey  string `json:"offset_key"`
	AtMs       int64  `json:"at_ms"`
	BoxState   int32  `json:"box_state,omitzero"`
	DoneBytes  uint64 `json:"done_bytes,omitzero"`
	TotalBytes uint64 `json:"total_bytes,omitzero"`
}

// EnableLedger copies every box head the state layer commits and every operation event into l until ctx ends.
func (s *Service) EnableLedger(ctx context.Context, l Ledger) {
	s.ledger, s.mirror = l, cluster.NewMirror()
	go s.mirror.Run(ctx)
	s.ops.mirror = func(rec cluster.Operation) {
		s.mirror.Put("op:"+rec.ID, func(ctx context.Context) error { return l.PutOperation(ctx, rec) })
	}
	if s.state != nil {
		s.state.mirror = func(p statePath, h *head) {
			key := "head:" + p.user + "/" + p.box
			if h == nil {
				s.mirror.Put(key, func(ctx context.Context) error { return l.DeleteHeads(ctx, p.user, p.box) })
				return
			}
			raw, err := json.Marshal(h)
			if err != nil {
				return
			}
			s.mirror.Put(key, func(ctx context.Context) error {
				_, err := l.PutHead(ctx, p.user, p.box, raw)
				return err
			})
		}
	}
}

// Verify rewrites the ledger heads and operations that differ from this process's, and purges operations past their retention.
func (s *Service) Verify(ctx context.Context) (Drift, error) {
	var d Drift
	if s.ledger == nil {
		return d, nil
	}
	if err := s.mirror.Flush(ctx); err != nil {
		return d, err
	}
	heads, backfill, err := s.headDifferences(ctx)
	if err != nil {
		return d, err
	}
	ops, err := s.operationDifferences(ctx)
	if err != nil {
		return d, err
	}
	if len(heads)+len(ops) > 0 {
		if err = s.mirror.Flush(ctx); err != nil {
			return d, err
		}
		if heads, _, err = s.headDifferences(ctx); err != nil {
			return d, err
		}
		if ops, err = s.operationDifferences(ctx); err != nil {
			return d, err
		}
	}
	for _, p := range slices.Concat(heads, backfill) {
		s.state.mu.Lock()
		h := s.state.heads[p]
		s.state.mu.Unlock()
		if h != (head{}) {
			s.state.mirror(p, &h)
		} else {
			s.state.mirror(p, nil)
		}
	}
	for _, id := range ops {
		if op := s.ops.get(id); op != nil {
			op.mu.Lock()
			op.mirrorLocked()
			op.mu.Unlock()
		}
	}
	for _, p := range heads {
		d.Heads = append(d.Heads, p.user+"/"+p.box)
	}
	d.Operations, d.Backfilled = ops, len(backfill)
	return d, s.ledger.PurgeOperations(ctx, time.Now().Add(-operationRetention))
}

// headDifferences returns the cached heads whose ledger row differs, and those the ledger lacks.
func (s *Service) headDifferences(ctx context.Context) ([]statePath, []statePath, error) {
	if s.state == nil {
		return nil, nil, nil
	}
	rows, err := s.ledger.Heads(ctx)
	if err != nil {
		return nil, nil, err
	}
	have := make(map[statePath]head, len(rows))
	for _, r := range rows {
		var h head
		if err := json.Unmarshal(r.Head, &h); err != nil {
			return nil, nil, fmt.Errorf("decode the ledger head of %s: %w", r.Box, err)
		}
		have[statePath{user: r.User, box: r.Box}] = h
	}
	s.state.mu.Lock()
	cached := maps.Clone(s.state.heads)
	s.state.mu.Unlock()
	var differ, missing []statePath
	for _, p := range slices.SortedFunc(maps.Keys(cached), comparePaths) {
		switch h, ok := have[p]; {
		case !ok && cached[p] != (head{}):
			missing = append(missing, p)
		case ok && h != cached[p]:
			differ = append(differ, p)
		}
	}
	return differ, missing, nil
}

// operationDifferences returns the ids of the operations whose ledger row is missing or differs.
func (s *Service) operationDifferences(ctx context.Context) ([]string, error) {
	since := time.Now().Add(-operationRetention)
	rows, err := s.ledger.Operations(ctx, since)
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
		got, ok := have[want.ID]
		if !ok || !sameOperation(want, got) {
			differ = append(differ, want.ID)
		}
	}
	slices.Sort(differ)
	return differ, nil
}

func sameOperation(a, b cluster.Operation) bool {
	var ea, eb []eventRecord
	if json.Unmarshal(a.Events, &ea) != nil || json.Unmarshal(b.Events, &eb) != nil {
		return false
	}
	return a.User == b.User && a.Box == b.Box && a.Phase == b.Phase && a.Park == b.Park && a.Gen == b.Gen &&
		a.FinishedAt.IsZero() == b.FinishedAt.IsZero() && slices.Equal(ea, eb)
}

func comparePaths(a, b statePath) int {
	return cmp.Or(cmp.Compare(a.user, b.user), cmp.Compare(a.box, b.box))
}

func toEventRecords(events []*controlv1.OperationEvent) ([]byte, error) {
	recs := make([]eventRecord, len(events))
	for i, ev := range events {
		recs[i] = eventRecord{
			Phase: int32(ev.GetPhase()), Detail: ev.GetDetail(), OffsetKey: ev.GetOffsetKey(), AtMs: ev.GetAtMs(),
			BoxState: int32(ev.GetBoxState()), DoneBytes: ev.GetDoneBytes(), TotalBytes: ev.GetTotalBytes(),
		}
	}
	raw, err := json.Marshal(recs)
	if err != nil {
		return nil, fmt.Errorf("encode events: %w", err)
	}
	return raw, nil
}
