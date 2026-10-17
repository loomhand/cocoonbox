package boxes

import (
	"context"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/cluster"
)

// EnableShared makes the shared tables the truth for box heads and operations until ctx ends.
func (s *Service) EnableShared(ctx context.Context, shared *cluster.Store) {
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
