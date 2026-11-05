package boxes

import (
	"context"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"

	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	metaRestore  = "restore"
	metaRestored = "restored"
)

// restoreMissing fills m, created because user's box had no machine, from the box's latest saved state.
func (s *Service) restoreMissing(ctx context.Context, user, box string, m machines.Machine, saved head) *controlv1.EnsureResponse {
	op, started := s.ops.start(context.WithoutCancel(ctx), user, box, controlv1.Phase_PHASE_RESTORING, m.Revision, stateTimeout)
	if started {
		go s.runRestore(op, user, m, saved)
	}
	return &controlv1.EnsureResponse{
		Box:       s.toBox(ctx, user, box, m),
		Operation: &controlv1.Operation{Id: op.id, BoxId: box},
	}
}

// runRestore boots m and restores saved into it; clients reach m only after, and a failure deletes m so the next Ensure restores again.
func (s *Service) runRestore(op *operation, user string, m machines.Machine, saved head) {
	logger := log.WithFunc("boxes.Service.runRestore").WithField("box", op.boxID)
	unpin, err := s.state.pin(op.ctx, statePath{user: user, box: op.boxID}, saved.Latest)
	defer unpin()
	if err != nil {
		logger.Error(op.ctx, err, "pin the snapshot to restore")
		s.discard(op, m.ID)
		op.fail("pin the snapshot failed")
		return
	}
	start := time.Now()
	savedAt := time.UnixMilli(saved.SavedAtMs).UTC().Format(time.RFC3339)
	op.note(controlv1.Phase_PHASE_CREATING, controlv1.BoxState_BOX_STATE_STARTING,
		fmt.Sprintf("machine %s for the state saved at %s", m.ID, savedAt))
	running, err := s.waitReady(op, m)
	if err != nil {
		logger.Error(op.ctx, err, "start machine for restore")
		s.discard(op, m.ID)
		op.fail(startFailure(err))
		return
	}
	booted := time.Since(start)

	ep, err := s.machines.Address(op.ctx, m.ID)
	var rec *snapshotRecord
	if err == nil {
		rec, err = s.state.restore(s.agentContext(op.ctx, running), statePath{user: user, box: op.boxID}, ep, saved.Latest, progressNotes(op, controlv1.Phase_PHASE_RESTORING, "restoring"))
	}
	if err != nil {
		logger.Error(op.ctx, err, "restore into new machine")
		s.discard(op, m.ID)
		op.fail("restore failed")
		return
	}
	if err := s.machines.SetMeta(op.ctx, m.ID, metaRestored, saved.Latest); err != nil {
		logger.Error(op.ctx, err, "record the restore of "+m.ID)
		op.fail("restored, but recording it failed; the next Ensure restores again")
		return
	}
	logger.Warnf(op.ctx, "restored %s from snapshot %s saved at %s; anything written after that save is not in the box", m.ID, rec.ID, savedAt)
	op.succeed(fmt.Sprintf("restored the state saved at %s (snapshot %s, %.1f MiB logical) into %s; anything written after that save is not in this box; boot %s, restore %s",
		savedAt, rec.ID, float64(rec.LogicalBytes)/mib, m.ID, booted.Round(time.Second), (time.Since(start) - booted).Round(time.Second)))
}

// restoring reports whether box is being filled from its saved state, so no client or save may reach its machine yet.
func (s *Service) restoring(box string) bool {
	active := s.ops.activeFor(box)
	return active != nil && active.phase == controlv1.Phase_PHASE_RESTORING
}

func (s *Service) startable(ctx context.Context, user, box string) (map[string]machines.Machine, error) {
	current, err := s.currentMachines(ctx, user)
	if err != nil {
		return nil, err
	}
	cur, ok := current[box]
	switch {
	case ok && cur.State == machines.StateHibernating:
		return nil, errHibernating
	case !ok || !unrestored(cur) || s.restoring(box):
		return current, nil
	}
	log.WithFunc("boxes.Service.startable").WithField("box", box).Warnf(ctx, "machine %s holds an unfinished restore; restoring into a new machine", cur.ID)
	if err = s.deleteMachine(ctx, cur.ID); err != nil {
		return nil, err
	}
	delete(current, box)
	return current, nil
}

// savedHead returns the latest saved state of user's box, empty without a state layer.
func (s *Service) savedHead(ctx context.Context, user, box string) (head, error) {
	if s.state == nil {
		return head{}, nil
	}
	return s.state.head(ctx, statePath{user: user, box: box})
}

func unrestored(m machines.Machine) bool {
	return m.Meta[metaRestore] != "" && m.Meta[metaRestored] != m.Meta[metaRestore]
}
