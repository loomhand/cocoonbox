package boxes

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/loomhand/cocoonbox/api/cluster"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
)

const (
	headKey = "head:"
	opKey   = "op:"
)

// Ledger keeps the box heads and operations api processes share.
type Ledger interface {
	Heads(ctx context.Context) ([]cluster.Head, error)
	PutHead(ctx context.Context, user, box string, head []byte) error
	DeleteHeads(ctx context.Context, user, box string) error
	Operations(ctx context.Context, since time.Time) ([]cluster.Operation, error)
	PutOperation(ctx context.Context, op cluster.Operation) error
	PurgeOperations(ctx context.Context, before time.Time) error
}

// Drift is what a Verify pass found the ledger holding wrong or missing.
type Drift struct {
	Heads      []string
	Operations []string
}

// EnableLedger copies every box head the state layer commits and every operation event into l until ctx ends.
func (s *Service) EnableLedger(ctx context.Context, l Ledger) {
	s.ledger, s.mirror = l, cluster.NewMirror()
	go s.mirror.Run(ctx)
	s.ops.mirror = func(rec cluster.Operation) {
		s.mirror.Put(opKey+rec.ID, func(ctx context.Context) error { return l.PutOperation(ctx, rec) })
	}
	if s.state != nil {
		s.state.mirror = func(p statePath, h *head) {
			key := p.ledgerKey()
			if h == nil {
				s.mirror.Put(key, func(ctx context.Context) error { return l.DeleteHeads(ctx, p.user, p.box) })
				return
			}
			raw := headBytes(*h)
			s.mirror.Put(key, func(ctx context.Context) error { return l.PutHead(ctx, p.user, p.box, raw) })
		}
	}
}

// Verify rewrites the ledger heads and operations that differ from this process's, and purges operations past their retention.
func (s *Service) Verify(ctx context.Context) (Drift, error) {
	var d Drift
	if s.ledger == nil {
		return d, nil
	}
	differ, err := s.mirror.Verify(ctx, s.differences)
	if err != nil {
		return d, err
	}
	for _, key := range differ {
		if id, ok := strings.CutPrefix(key, opKey); ok {
			d.Operations = append(d.Operations, id)
			if op := s.ops.get(id); op != nil {
				op.mu.Lock()
				op.mirrorLocked()
				op.mu.Unlock()
			}
			continue
		}
		name := strings.TrimPrefix(key, headKey)
		d.Heads = append(d.Heads, name)
		user, box, _ := strings.Cut(name, "/")
		p := statePath{user: user, box: box}
		s.state.mu.Lock()
		h := s.state.heads[p]
		s.state.mu.Unlock()
		if h != (head{}) {
			s.state.mirror(p, &h)
		} else {
			s.state.mirror(p, nil)
		}
	}
	return d, s.ledger.PurgeOperations(ctx, time.Now().Add(-operationRetention))
}

// differences returns the keys of the cached heads and the operations whose ledger row differs or is missing.
func (s *Service) differences(ctx context.Context) ([]string, error) {
	heads, err := s.headDifferences(ctx)
	if err != nil {
		return nil, err
	}
	ops, err := s.operationDifferences(ctx)
	return slices.Concat(heads, ops), err
}

func (s *Service) headDifferences(ctx context.Context) ([]string, error) {
	if s.state == nil {
		return nil, nil
	}
	rows, err := s.ledger.Heads(ctx)
	if err != nil {
		return nil, err
	}
	have := make(map[statePath][]byte, len(rows))
	for _, r := range rows {
		have[statePath{user: r.User, box: r.Box}] = r.Head
	}
	s.state.mu.Lock()
	cached := maps.Clone(s.state.heads)
	s.state.mu.Unlock()
	var differ []string
	for _, p := range slices.SortedFunc(maps.Keys(cached), comparePaths) {
		stored, ok := have[p]
		if h := cached[p]; ok != (h != head{}) || ok && !bytes.Equal(stored, headBytes(h)) {
			differ = append(differ, p.ledgerKey())
		}
	}
	return differ, nil
}

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

func comparePaths(a, b statePath) int {
	return cmp.Or(cmp.Compare(a.user, b.user), cmp.Compare(a.box, b.box))
}

func eventRecords(events []*controlv1.OperationEvent) []byte {
	recs := make([]eventRecord, len(events))
	for i, ev := range events {
		recs[i] = eventRecord{
			Phase: int32(ev.GetPhase()), Detail: ev.GetDetail(), OffsetKey: ev.GetOffsetKey(), AtMs: ev.GetAtMs(),
			BoxState: int32(ev.GetBoxState()), DoneBytes: ev.GetDoneBytes(), TotalBytes: ev.GetTotalBytes(),
		}
	}
	raw, _ := json.Marshal(recs)
	return raw
}
