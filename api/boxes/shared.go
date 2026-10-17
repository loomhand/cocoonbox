package boxes

import (
	"context"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/cluster"
)

// SharedHeads is the box head table api processes share.
type SharedHeads interface {
	Head(ctx context.Context, user, box string) (cluster.Head, error)
	SwapHead(ctx context.Context, user, box string, head []byte, version int64) (int64, error)
}

// Shared is the state api processes share, with the notifies of its changes.
type Shared interface {
	SharedHeads
	SharedOperations
	Listen(ctx context.Context, channel string, reset func(), fn cluster.Notified)
}

// EnableShared makes the shared tables the truth for box heads and operations: a save commits its head on the version
// it planned on, and the operations of other processes count as running here until ctx ends.
func (s *Service) EnableShared(ctx context.Context, shared Shared) {
	logger := log.WithFunc("boxes.Service.EnableShared")
	s.ops.mu.Lock()
	s.ops.shared = shared
	if s.mirror != nil {
		s.ops.mirror = func(rec cluster.Operation) {
			s.mirror.Put("op:"+rec.ID, func(ctx context.Context) error { return shared.PutRunningOperation(ctx, rec) })
		}
	}
	s.ops.mu.Unlock()
	go s.ops.renew(ctx)
	go shared.Listen(ctx, cluster.ChannelOperations, func() {
		if err := s.ops.reload(ctx); err != nil {
			logger.Warnf(ctx, "reload the shared operations: %v", err)
		}
	}, func(payload string) {
		c, err := cluster.ParseChange(payload)
		switch {
		case err != nil:
			logger.Warnf(ctx, "take an operations notify: %v", err)
		case c.Finished || !s.ops.runsHere(c.ID):
			s.ops.notified(ctx, c.ID)
		}
	})
	if s.state == nil {
		return
	}
	s.state.shared = shared
	go shared.Listen(ctx, cluster.ChannelHeads, s.state.invalidateAll, func(payload string) {
		row, err := cluster.ParseHeadNotify(payload)
		switch {
		case err != nil:
			logger.Warnf(ctx, "take a heads notify: %v", err)
		case row.Version == 0:
			s.state.forgetCache(statePath{user: row.User, box: row.Box})
		default:
			if err := s.state.learnHead(row); err != nil {
				logger.Warnf(ctx, "take a heads notify: %v", err)
			}
		}
	})
}
