package boxes

import (
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
)

func (s *Service) Watch(_ *controlv1.WatchBoxesRequest, stream grpc.ServerStreamingServer[controlv1.ListBoxesResponse]) error {
	ctx := stream.Context()
	user, err := callerUser(ctx)
	if err != nil {
		return err
	}
	var last *controlv1.ListBoxesResponse
	for {
		boxesChanged, machinesChanged := s.ops.watchers.changes(user), s.machines.Changes(user)
		list, err := s.List(ctx, &controlv1.ListBoxesRequest{})
		if err != nil {
			return err
		}
		if !proto.Equal(list, last) {
			if err := stream.Send(list); err != nil {
				return err
			}
			last = list
		}
		select {
		case <-boxesChanged:
		case <-machinesChanged:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

// watchers wakes the Watch streams of an owner.
type watchers struct {
	mu   sync.Mutex
	next map[string]chan struct{}
}

// changes returns a channel closed on the next wake of owner.
func (w *watchers) changes(owner string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch := w.next[owner]
	if ch == nil {
		ch = make(chan struct{})
		w.next[owner] = ch
	}
	return ch
}

func (w *watchers) wake(owner string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ch := w.next[owner]; ch != nil {
		close(ch)
		delete(w.next, owner)
	}
}
