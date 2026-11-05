package boxes

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/auth"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

const probeTimeout = 15 * time.Second

// Activity tells whether work runs in a box that no client reaches: an agent turn, or an offloaded CLI session at work.
type Activity interface {
	Busy(ctx context.Context, box string, ep machines.Endpoint) (bool, error)
}

// EnableIdle hibernates a running box with no open client call and none begun or ended for agent (an agent's box) or user (the user's own), or squeezed when shorter on a full node; zero leaves it up.
func (s *Service) EnableIdle(agent, user, squeezed time.Duration) {
	s.idle = &idling{agent: agent, user: user, squeezed: squeezed, probe: probeTimeout, now: time.Now, last: map[string]time.Time{}, open: map[string]int{}}
}

// EnableActivity asks activity before an idle user box hibernates; a box it calls busy, or cannot answer for, stays up.
func (s *Service) EnableActivity(activity Activity) {
	s.activity = activity
}

// RunIdle hibernates the idle boxes every interval until ctx ends.
func (s *Service) RunIdle(ctx context.Context, interval time.Duration) {
	every(ctx, interval, func() { s.hibernateIdle(ctx) })
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
	s.readReaches(ctx)
	for _, box := range slices.Sorted(maps.Keys(byBox)) {
		m := byBox[box][0]
		agent := s.agentBox(m.Owner, box)
		after := s.idle.threshold(agent, m.Squeezed)
		if after == 0 || len(byBox[box]) > 1 || m.State != machines.StateRunning || !s.ready(m) || s.ops.activeFor(box) != nil ||
			!s.registry.HasBox(m.Owner, box) || !s.idle.idleAt(box, now, after, s.reachedElsewhere(box)) {
			continue
		}
		if !agent {
			s.sleepIdle(ctx, m, after)
			continue
		}
		if !s.stillIdle(ctx, box, after) {
			continue
		}
		if _, err := s.Hibernate(auth.WithUser(ctx, m.Owner), &controlv1.BoxRef{BoxId: box}); err != nil {
			logger.Warnf(ctx, "hibernate idle agent box %s: %v", box, err)
			continue
		}
		logger.Infof(ctx, "hibernating agent box %s of user %s, idle for %s", box, m.Owner, after)
	}
}

// sleepIdle saves the idle user box of m and hibernates it, unless a scheduled wake or work in it keeps it up.
func (s *Service) sleepIdle(ctx context.Context, m machines.Machine, after time.Duration) {
	box := boxOfMachine(m.ID)
	logger := log.WithFunc("boxes.Service.sleepIdle").WithField("box", box)
	switch reason, err := s.keepAwake(ctx, m); {
	case err != nil:
		logger.Warnf(ctx, "idle box stays up: %v", err)
		return
	case reason != "":
		logger.Infof(ctx, "idle box stays up: %s", reason)
		return
	}
	op, started := s.ops.start(context.WithoutCancel(ctx), m.Owner, box, controlv1.Phase_PHASE_SAVING, m.Revision, stateTimeout)
	if !started {
		return
	}
	logger.Infof(ctx, "box of user %s idle for %s; saving it before it hibernates as operation %s", m.Owner, after, op.id)
	go s.runIdleSleep(op, m, after)
}

// keepAwake names why idle box m stays up, its scheduled wake or work in it, or fails with the check that failed; empty lets it sleep.
func (s *Service) keepAwake(ctx context.Context, m machines.Machine) (string, error) {
	box := boxOfMachine(m.ID)
	if s.wake != nil {
		list, err := s.wake.store.WakeSchedules(ctx, m.Owner)
		if err != nil {
			return "", fmt.Errorf("read its wake schedule: %w", err)
		}
		if slices.ContainsFunc(list, func(w users.WakeSchedule) bool { return w.Box == box && w.SleepAt.After(s.idle.now()) }) {
			return "its scheduled wake is on", nil
		}
	}
	if s.activity == nil {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.idle.probe)
	defer cancel()
	ep, err := s.machines.Address(ctx, m.ID)
	if err != nil {
		return "", fmt.Errorf("address it: %w", err)
	}
	busy, err := s.activity.Busy(s.agentContext(ctx, m), box, ep)
	switch {
	case err != nil:
		return "", fmt.Errorf("ask its agent: %w", err)
	case busy:
		return "an agent turn or an offloaded session works in it", nil
	}
	return "", nil
}

func (s *Service) stillIdle(ctx context.Context, box string, after time.Duration) bool {
	s.readReaches(ctx)
	return s.idle.idleAt(box, s.idle.now(), after, s.reachedElsewhere(box))
}

func (s *Service) runIdleSleep(op *operation, m machines.Machine, after time.Duration) {
	logger := log.WithFunc("boxes.Service.runIdleSleep").WithField("box", op.boxID)
	detail := "saved the idle box; hibernating it"
	if s.state != nil {
		if _, err := s.save(op, op.user, m, agentv1.Consistency_CONSISTENCY_LIVE); err != nil {
			logger.Error(op.ctx, err, "save the idle box; its hibernation keeps the state")
			detail = "save failed; hibernating it"
		}
	}
	if !s.stillIdle(op.ctx, op.boxID, after) {
		op.succeed("a client came back while saving; the box stays up")
		return
	}
	op.succeed(detail)
	ctx := context.WithoutCancel(op.ctx)
	if _, err := s.Hibernate(auth.WithUser(ctx, op.user), &controlv1.BoxRef{BoxId: op.boxID}); err != nil {
		logger.Warnf(ctx, "hibernate the idle box: %v", err)
		return
	}
	logger.Infof(ctx, "hibernating the box of user %s, idle for %s", op.user, after)
}

// idling keeps, per box, how many client calls are open and when the last one began or ended.
type idling struct {
	agent    time.Duration
	user     time.Duration
	squeezed time.Duration
	probe    time.Duration
	now      func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
	open map[string]int
}

func (i *idling) threshold(agent, squeezed bool) time.Duration {
	after := i.user
	if agent {
		after = i.agent
	}
	if squeezed && after > 0 && i.squeezed > 0 {
		return min(after, i.squeezed)
	}
	return after
}

// enter records a call to box that runs until the returned func; ended, when set, learns when it ended.
func (i *idling) enter(box string, ended reachFunc) func() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.last[box] = i.now()
	i.open[box]++
	return func() {
		i.mu.Lock()
		at := i.now()
		i.last[box] = at
		if i.open[box]--; i.open[box] <= 0 {
			delete(i.open, box)
		}
		i.mu.Unlock()
		if ended != nil {
			ended(box, at)
		}
	}
}

// idleAt reports whether box had no open call here for after nor a reach elsewhere since; a box seen for the first time starts its idle time now.
func (i *idling) idleAt(box string, now time.Time, after time.Duration, elsewhere time.Time) bool {
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
	if elsewhere.After(at) {
		at = elsewhere
	}
	return now.Sub(at) >= after
}

// opened returns the boxes that open calls hold.
func (i *idling) opened() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Collect(maps.Keys(i.open))
}

// keep forgets the last call of every box that is not running, so a box's idle time starts again at its next wake.
func (i *idling) keep(running map[string]bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	maps.DeleteFunc(i.last, func(box string, _ time.Time) bool { return !running[box] && i.open[box] == 0 })
}
