package boxes

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

// RecordFunc records the box an agent's Create added, before its machine starts.
type RecordFunc func(ctx context.Context, box string) error

type agentKey struct{}

// WithAgent marks the Creates of ctx as an agent's: the box counts against the agent's caps instead of the owner's max
// boxes and running limit, gets none of the owner's credentials, and record runs on it before its machine starts; a
// record error removes the box and fails the Create.
func WithAgent(ctx context.Context, record RecordFunc) context.Context {
	return context.WithValue(ctx, agentKey{}, record)
}

// AgentRecord returns the RecordFunc of an agent's ctx, nil for the owner's.
func AgentRecord(ctx context.Context) RecordFunc {
	record, _ := ctx.Value(agentKey{}).(RecordFunc)
	return record
}

// EnableIdle hibernates a running agent box once no client call to it is open and none ended for after.
func (s *Service) EnableIdle(after time.Duration) {
	s.idle = &idling{after: after, now: time.Now, last: map[string]time.Time{}, open: map[string]int{}}
}

// RunIdle hibernates the idle agent boxes every interval until ctx ends.
func (s *Service) RunIdle(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.hibernateIdle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) hibernateIdle(ctx context.Context) {
	logger := log.WithFunc("boxes.Service.hibernateIdle")
	ms, err := s.machines.List(ctx, "")
	if err != nil {
		logger.Error(ctx, err, "list machines")
		return
	}
	byBox := map[string][]machines.Machine{}
	running := map[string]bool{}
	for _, m := range ms {
		byBox[boxOfMachine(m.ID)] = append(byBox[boxOfMachine(m.ID)], m)
		running[boxOfMachine(m.ID)] = running[boxOfMachine(m.ID)] || m.State == machines.StateRunning
	}
	now := s.idle.now()
	s.idle.keep(running)
	for _, box := range slices.Sorted(maps.Keys(byBox)) {
		m := byBox[box][0]
		if !s.agentBox(m.Owner, box) || len(byBox[box]) > 1 || m.State != machines.StateRunning || !s.ready(m) || s.ops.activeFor(box) != nil ||
			!s.registry.HasBox(m.Owner, box) || !s.idle.idleAt(box, now) {
			continue
		}
		if _, err := s.Hibernate(auth.WithUser(ctx, m.Owner), &controlv1.BoxRef{BoxId: box}); err != nil {
			logger.Warnf(ctx, "hibernate idle agent box %s: %v", box, err)
			continue
		}
		logger.Infof(ctx, "hibernating agent box %s of user %s, idle for %s", box, m.Owner, s.idle.after)
	}
}

// agentBox reports whether owner's box is one an agent created.
func (s *Service) agentBox(owner, box string) bool {
	return slices.ContainsFunc(s.registry.Boxes(owner), func(b users.Box) bool { return b.ID == box && b.Agent })
}

// ownerMachines drops the machines of agent boxes from ms.
func (s *Service) ownerMachines(ms []machines.Machine) []machines.Machine {
	return slices.DeleteFunc(slices.Clone(ms), func(m machines.Machine) bool { return s.agentBox(m.Owner, boxOfMachine(m.ID)) })
}

// idling keeps, per box, how many client calls are open and when the last one began or ended.
type idling struct {
	after time.Duration
	now   func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
	open map[string]int
}

func (i *idling) enter(box string) func() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.last[box] = i.now()
	i.open[box]++
	return func() {
		i.mu.Lock()
		defer i.mu.Unlock()
		i.last[box] = i.now()
		if i.open[box]--; i.open[box] <= 0 {
			delete(i.open, box)
		}
	}
}

// idleAt reports whether box had no open call for after; a box seen for the first time starts its idle time now.
func (i *idling) idleAt(box string, now time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.open[box] > 0 {
		return false
	}
	at, ok := i.last[box]
	if !ok {
		i.last[box] = now
		return false
	}
	return now.Sub(at) >= i.after
}

// keep forgets the last call of every box that is not running, so a box's idle time starts again at its next wake.
func (i *idling) keep(running map[string]bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	maps.DeleteFunc(i.last, func(box string, _ time.Time) bool { return !running[box] && i.open[box] == 0 })
}
