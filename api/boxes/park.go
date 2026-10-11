package boxes

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/projecteru2/core/log"

	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

// metaSlept records when the api last hibernated a machine, in Unix milliseconds.
const metaSlept = "slept"

// EnableParking parks every box of owners, or of everyone when owners is empty, hibernated longer than after: RunParking saves it and removes its machine, and the next Ensure restores it.
func (s *Service) EnableParking(after time.Duration, owners []string) {
	s.park = &parking{after: after, owners: owners, now: time.Now}
}

// RunParking parks the boxes idle past the threshold every interval until ctx ends.
func (s *Service) RunParking(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.parkIdle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// parkIdle parks the idle boxes one at a time, so the sweep wakes at most one machine at once.
func (s *Service) parkIdle(ctx context.Context) {
	logger := log.WithFunc("boxes.Service.parkIdle")
	ms, err := s.machines.List(ctx, "")
	if err != nil {
		logger.Error(ctx, err, "list machines")
		return
	}
	now := s.park.now()
	held, err := s.wakesDue(ctx, now)
	if err != nil {
		logger.Error(ctx, err, "list wake schedules")
		return
	}
	byBox := map[string][]machines.Machine{}
	for _, m := range ms {
		byBox[boxOfMachine(m.ID)] = append(byBox[boxOfMachine(m.ID)], m)
	}
	for _, box := range slices.Sorted(maps.Keys(byBox)) {
		m := byBox[box][0]
		if len(byBox[box]) > 1 || held[box] || m.State != machines.StateHibernated || !s.park.covers(m.Owner) || !s.registry.HasBox(m.Owner, box) {
			continue
		}
		if now.Sub(s.idleSince(ctx, box, m)) < s.park.after {
			continue
		}
		op := s.startPark(ctx, box, m)
		if op == nil {
			continue
		}
		select {
		case <-op.ctx.Done():
		case <-ctx.Done():
			return
		}
	}
}

// wakesDue returns the boxes whose wake schedule fires within the threshold or still holds them awake.
func (s *Service) wakesDue(ctx context.Context, now time.Time) (map[string]bool, error) {
	held := map[string]bool{}
	if s.wake == nil {
		return held, nil
	}
	list, err := s.wake.store.WakeSchedules(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, w := range list {
		if !w.SleepAt.IsZero() || w.NextAt.Before(now.Add(s.park.after)) {
			held[w.Box] = true
		}
	}
	return held, nil
}

// idleSince is the last time m of box is known to run: its hibernation stamp, its latest save or its claim, whichever is latest.
func (s *Service) idleSince(ctx context.Context, box string, m machines.Machine) time.Time {
	ms := s.state.lastSaved(ctx, statePath{user: m.Owner, box: box})
	if slept, err := strconv.ParseInt(m.Meta[metaSlept], 10, 64); err == nil {
		ms = max(ms, slept)
	}
	return later(time.UnixMilli(ms), m.CreatedAt)
}

// startPark wakes m, the only machine of box, and runs its park as an operation; nil when the box changed since the sweep read it.
func (s *Service) startPark(ctx context.Context, box string, m machines.Machine) *operation {
	logger := log.WithFunc("boxes.Service.startPark").WithField("box", box)
	unlock := s.lockStarts(m.Owner)
	defer unlock()
	cur, err := s.current(ctx, m.Owner, box)
	if err != nil || cur.ID != m.ID || cur.State != machines.StateHibernated || s.ops.activeFor(box) != nil {
		return nil
	}
	op, _ := s.ops.begin(context.WithoutCancel(ctx), &operation{user: cur.Owner, boxID: box, phase: controlv1.Phase_PHASE_SAVING, park: true, gen: cur.Revision}, stateTimeout)
	woken, action, err := s.machines.Ensure(op.ctx, machines.Spec{ID: cur.ID, Owner: cur.Owner, Flavor: cur.Flavor})
	if err == nil && action != machines.ActionWoken {
		err = fmt.Errorf("machine %s did not wake (action %d)", cur.ID, action)
		if action == machines.ActionCreated {
			s.discard(op, cur.ID)
		}
	}
	if err != nil {
		logger.Error(ctx, err, "wake to park")
		op.fail("wake failed")
		return op
	}
	s.wakeCredentials(ctx, woken)
	logger.Infof(ctx, "parking %s as operation %s", cur.ID, op.id)
	go s.runPark(op, woken)
	return op
}

// runPark saves m quiesced and removes it; a client's Ensure or Hibernate supersedes op and keeps m.
func (s *Service) runPark(op *operation, m machines.Machine) {
	logger := log.WithFunc("boxes.Service.runPark").WithField("box", op.boxID)
	start := time.Now()
	op.note(controlv1.Phase_PHASE_WAKING, controlv1.BoxState_BOX_STATE_STARTING, "waking "+m.ID+" to save it before parking")
	running, err := s.waitRunning(op, m)
	if err == nil && s.provision != nil {
		if err = s.provisionMachine(op.ctx, running); err == nil {
			running, err = s.machines.Get(op.ctx, m.ID)
		}
	}
	var rec *snapshotRecord
	if err == nil {
		rec, err = s.save(op, op.user, running, agentv1.Consistency_CONSISTENCY_QUIESCED)
	}
	unlock := s.lockStarts(op.user)
	defer unlock()
	if s.ops.activeFor(op.boxID) != op {
		logger.Infof(op.ctx, "a client took %s over; it stays", m.ID)
		return
	}
	if err == nil {
		err = s.deleteMachine(op.ctx, m.ID)
	}
	if err != nil {
		logger.Error(op.ctx, err, "park "+m.ID)
		if _, herr := s.sleep(context.WithoutCancel(op.ctx), m.ID); herr != nil {
			logger.Error(op.ctx, herr, "hibernate "+m.ID+" again")
		}
		op.fail("park failed; the box is hibernated again")
		return
	}
	logger.Infof(op.ctx, "parked: removed %s after snapshot %s", m.ID, rec.ID)
	op.succeed(fmt.Sprintf("parked: snapshot %s, %.1f MiB logical, %.1f MiB uploaded; machine %s removed in %s; the next Ensure restores it",
		rec.ID, float64(rec.LogicalBytes)/mib, float64(rec.UploadedBytes)/mib, m.ID, time.Since(start).Round(time.Second)))
}

func (s *Service) parking(box string) bool {
	active := s.ops.activeFor(box)
	return active != nil && active.park
}

// sleep hibernates machine id, stamping the time first when parking is on.
func (s *Service) sleep(ctx context.Context, id string) (machines.Machine, error) {
	if s.park != nil {
		if err := s.machines.SetMeta(ctx, id, metaSlept, strconv.FormatInt(s.park.now().UnixMilli(), 10)); err != nil {
			log.WithFunc("boxes.Service.sleep").WithField("machine", id).Warnf(ctx, "stamp the hibernation: %v", err)
		}
	}
	return s.machines.Hibernate(ctx, id)
}

type parking struct {
	after  time.Duration
	owners []string
	now    func() time.Time
}

func (p *parking) covers(owner string) bool {
	return len(p.owners) == 0 || slices.Contains(p.owners, owner)
}
