package boxes

import (
	"context"
	"strings"

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
	Listen(ctx context.Context, channel string, reset func(), fn func(payload string))
}

// EnableShared makes the shared tables the truth for box heads and operations: a save commits its head on the version
// it planned on, and the operations of other processes count as running here until ctx ends.
func (s *Service) EnableShared(ctx context.Context, shared Shared) {
	logger := log.WithFunc("boxes.Service.EnableShared")
	done, cancel := context.WithCancel(ctx)
	cancel()
	s.ops.mu.Lock()
	s.ops.shared, s.ops.done = shared, done
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
	}, func(id string) { s.ops.notified(ctx, id) })
	if s.state == nil {
		return
	}
	s.state.shared = shared
	go shared.Listen(ctx, cluster.ChannelHeads, s.state.invalidateAll, func(payload string) {
		user, box, _ := strings.Cut(payload, "/")
		if box == "" {
			s.state.forgetCache(statePath{user: user})
			return
		}
		s.state.invalidate(statePath{user: user, box: box})
	})
}
