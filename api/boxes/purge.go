package boxes

import (
	"context"
	"maps"
	"slices"

	"github.com/CMGS/cocoonbox/api/store"
)

const purgeBatch = 1000

// DeleteState removes every object of user's state from st and returns how many it removed; a user without state has none.
func DeleteState(ctx context.Context, st store.Store, user string) (int, error) {
	listing, err := st.List(ctx, user+"/")
	if err != nil {
		return 0, err
	}
	keys := make([]string, len(listing))
	for i, obj := range listing {
		keys[i] = obj.Key
	}
	for part := range slices.Chunk(keys, purgeBatch) {
		if err := st.Delete(ctx, part); err != nil {
			return 0, err
		}
	}
	return len(keys), nil
}

// DeleteState removes user's saved state once its machines are released; a user without state is not an error.
func (s *Service) DeleteState(ctx context.Context, user string) error {
	if s.state == nil {
		return nil
	}
	exclusive := s.state.userLock(user)
	exclusive.Lock()
	defer exclusive.Unlock()
	if _, err := DeleteState(ctx, s.state.store, user); err != nil {
		return err
	}
	s.state.forget(user)
	return nil
}

func (l *stateLayer) forget(user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	maps.DeleteFunc(l.heads, func(p statePath, _ head) bool { return p.user == user })
	maps.DeleteFunc(l.browsed, func(p statePath, _ browsed) bool { return p.user == user })
	maps.DeleteFunc(l.bases, func(p statePath, _ *snapshotRecord) bool { return p.user == user })
}
