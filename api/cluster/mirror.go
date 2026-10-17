package cluster

import (
	"context"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
)

const mirrorWriteTimeout = 10 * time.Second

// Write is one queued write of a Mirror.
type Write func(ctx context.Context) error

// Mirror applies writes on one goroutine in the order their keys were first queued, keeping only the newest write per key.
type Mirror struct {
	mu       sync.Mutex
	pending  map[string]Write
	order    []string
	inflight bool
	changed  chan struct{}
}

// NewMirror returns a mirror that applies nothing until Run.
func NewMirror() *Mirror {
	return &Mirror{pending: map[string]Write{}, changed: make(chan struct{})}
}

// Put queues w for key, replacing a write for key not yet applied.
func (m *Mirror) Put(key string, w Write) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pending[key]; !ok {
		m.order = append(m.order, key)
	}
	m.pending[key] = w
	m.wakeLocked()
}

// Run applies the queued writes until ctx ends; a failed write is logged and dropped.
func (m *Mirror) Run(ctx context.Context) {
	logger := log.WithFunc("cluster.Mirror.Run")
	for {
		m.mu.Lock()
		changed := m.changed
		if len(m.order) == 0 {
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
			continue
		}
		key := m.order[0]
		m.order = m.order[1:]
		w := m.pending[key]
		delete(m.pending, key)
		m.inflight = true
		m.mu.Unlock()

		writeCtx, cancel := context.WithTimeout(ctx, mirrorWriteTimeout)
		if err := w(writeCtx); err != nil {
			logger.Warnf(ctx, "mirror %s: %v", key, err)
		}
		cancel()

		m.mu.Lock()
		m.inflight = false
		m.wakeLocked()
		m.mu.Unlock()
	}
}

// Verify returns the keys diff finds different once the queued writes landed, asking diff again for only those after a
// second flush, so a write queued during the first diff does not count.
func (m *Mirror) Verify(ctx context.Context, diff func(ctx context.Context, only []string) ([]string, error)) ([]string, error) {
	if err := m.Flush(ctx); err != nil {
		return nil, err
	}
	differ, err := diff(ctx, nil)
	if err != nil || len(differ) == 0 {
		return nil, err
	}
	if err := m.Flush(ctx); err != nil {
		return nil, err
	}
	return diff(ctx, differ)
}

// Flush waits until no write is queued or running.
func (m *Mirror) Flush(ctx context.Context) error {
	for {
		m.mu.Lock()
		if len(m.order) == 0 && !m.inflight {
			m.mu.Unlock()
			return nil
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (m *Mirror) wakeLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}
