package boxes

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/cluster"
)

const (
	reachFlush   = 30 * time.Second
	reachHorizon = 24 * time.Hour
)

// readReaches takes in the reaches of every process before a sweep, with a shared table.
func (s *Service) readReaches(ctx context.Context) {
	if s.reaches == nil {
		return
	}
	if err := s.reaches.read(ctx); err != nil {
		log.WithFunc("boxes.Service.readReaches").Warnf(ctx, "the sweep sees the reaches of its last read: %v", err)
	}
}

// reachedElsewhere is when a client of any process last reached box, as the last read saw it; zero without a shared table.
func (s *Service) reachedElsewhere(box string) time.Time {
	if s.reaches == nil {
		return time.Time{}
	}
	return s.reaches.last(box)
}

// reachedSince reports whether a client of any process reached box at or after t.
func (s *Service) reachedSince(box string, t time.Time) bool {
	at := s.reachedElsewhere(box)
	return s.wake.reachedSince(box, t) || !at.IsZero() && !at.Before(t)
}

type reachFunc func(box string, at time.Time)

// reaches carries this process's client reaches to the shared table and the other processes' reaches to the sweeps.
type reaches struct {
	shared *cluster.Store
	now    func() time.Time
	ctx    context.Context

	mu      sync.Mutex
	pending map[string]time.Time
	remote  map[string]time.Time
	// written holds the last reach of each box this process wrote.
	written map[string]time.Time
}

func newReaches(ctx context.Context, shared *cluster.Store, now func() time.Time) *reaches {
	return &reaches{shared: shared, now: now, ctx: ctx, pending: map[string]time.Time{}, remote: map[string]time.Time{}, written: map[string]time.Time{}}
}

// reach records a reach of box at, writing the first after a flush period at once so a sweep elsewhere sees it.
func (r *reaches) reach(box string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if at.Sub(r.written[box]) >= reachFlush {
		r.written[box] = at
		go func() {
			if err := r.shared.PutActivity(r.ctx, map[string]time.Time{box: at}); err != nil {
				r.reach(box, at)
			}
		}()
		return
	}
	if at.After(r.pending[box]) {
		r.pending[box] = at
	}
}

// last is when a client of any process last reached box, as the last read saw it.
func (r *reaches) last(box string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.remote[box]
}

// read takes in the reaches of every process and drops those past the horizon from the table.
func (r *reaches) read(ctx context.Context) error {
	since := r.now().Add(-reachHorizon)
	if err := r.shared.PurgeActivity(ctx, since); err != nil {
		return err
	}
	remote, err := r.shared.Activity(ctx, since)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remote = remote
	return nil
}

// run flushes every reachFlush until ctx ends.
func (r *reaches) run(ctx context.Context, open func() []string) {
	ticker := time.NewTicker(reachFlush)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		r.flush(ctx, open())
	}
}

// flush writes the reaches since the last flush, with the open boxes at now, and keeps them for the next on a failure.
func (r *reaches) flush(ctx context.Context, open []string) {
	logger := log.WithFunc("boxes.reaches.flush")
	now := r.now()
	r.mu.Lock()
	for _, box := range open {
		r.pending[box] = now
	}
	batch := maps.Clone(r.pending)
	clear(r.pending)
	maps.Copy(r.written, batch)
	maps.DeleteFunc(r.written, func(_ string, at time.Time) bool { return now.Sub(at) > reachHorizon })
	r.mu.Unlock()
	if err := r.shared.PutActivity(ctx, batch); err != nil {
		logger.Warnf(ctx, "write the reaches of %d boxes: %v", len(batch), err)
		r.mu.Lock()
		for box, at := range batch {
			if at.After(r.pending[box]) {
				r.pending[box] = at
			}
		}
		r.mu.Unlock()
	}
}
