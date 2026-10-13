package boxes

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

// AgentBoxes lists the boxes that agents created, which hibernate when idle.
type AgentBoxes func(ctx context.Context) (map[string]bool, error)

// EnableIdle hibernates a running box of agentBoxes once no client call to it is open and none ended for after.
func (s *Service) EnableIdle(after time.Duration, agentBoxes AgentBoxes) {
	s.idle = &idling{after: after, agentBoxes: agentBoxes, now: time.Now, last: map[string]time.Time{}, open: map[string]int{}}
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
	agents, err := s.idle.agentBoxes(ctx)
	if err != nil {
		logger.Error(ctx, err, "list agent boxes")
		return
	}
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
		if !agents[box] || len(byBox[box]) > 1 || m.State != machines.StateRunning || !s.ready(m) || s.ops.activeFor(box) != nil ||
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

// idling keeps, per box, how many client calls are open and when the last one began or ended.
type idling struct {
	after      time.Duration
	agentBoxes AgentBoxes
	now        func() time.Time

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
