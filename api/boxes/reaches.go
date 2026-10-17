package boxes

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
)

const (
	reachFlush   = 30 * time.Second
	reachHorizon = 24 * time.Hour
)

// SharedReaches keeps when a client last reached each box, across api processes.
type SharedReaches interface {
	PutActivity(ctx context.Context, reached map[string]time.Time) error
	Activity(ctx context.Context, since time.Time) (map[string]time.Time, error)
	PurgeActivity(ctx context.Context, before time.Time) error
}

// readReaches takes in the reaches of every process before a sweep, with a shared table.
func (s *Service) readReaches(ctx context.Context) {
	if s.reaches == nil {
		return
	}
	if err := s.reaches.read(ctx); err != nil {
		log.WithFunc("boxes.Service.readReaches").Warnf(ctx, "the sweep sees this process's reaches only: %v", err)
	}
}

// reachedElsewhere is when a client of any process last reached box, zero without a shared table.
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

// reaches carries this process's client reaches to the shared table and the other processes' reaches to the sweeps.
type reaches struct {
	shared SharedReaches
	now    func() time.Time

	mu      sync.Mutex
	pending map[string]time.Time
	remote  map[string]time.Time
}

func newReaches(shared SharedReaches, now func() time.Time) *reaches {
	return &reaches{shared: shared, now: now, pending: map[string]time.Time{}, remote: map[string]time.Time{}}
}

func (r *reaches) reach(box string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if at.After(r.pending[box]) {
		r.pending[box] = at
	}
}

// last is when a client of another process last reached box, as the last read saw it.
func (r *reaches) last(box string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.remote[box]
}

// read takes in the reaches of every process.
func (r *reaches) read(ctx context.Context) error {
	remote, err := r.shared.Activity(ctx, r.now().Add(-reachHorizon))
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
	for _, box := range open {
		r.reach(box, now)
	}
	r.mu.Lock()
	batch := maps.Clone(r.pending)
	clear(r.pending)
	r.mu.Unlock()
	if err := r.shared.PurgeActivity(ctx, now.Add(-reachHorizon)); err != nil {
		logger.Warnf(ctx, "purge the old reaches: %v", err)
	}
	if err := r.shared.PutActivity(ctx, batch); err != nil {
		logger.Warnf(ctx, "write the reaches of %d boxes: %v", len(batch), err)
		for box, at := range batch {
			r.reach(box, at)
		}
	}
}
