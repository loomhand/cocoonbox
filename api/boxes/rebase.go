package boxes

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

func (s *Service) Rebase(ctx context.Context, req *controlv1.RebaseRequest) (*controlv1.Operation, error) {
	box := req.GetBoxId()
	user, err := ownerOf(ctx, box)
	if err != nil {
		return nil, err
	}
	image := cmp.Or(req.GetImage(), s.defaults.Image)
	if image != s.defaults.Image && !slices.Contains(s.defaults.RebaseImages, image) {
		return nil, status.Errorf(codes.InvalidArgument, "image %q is not a rebase target", image)
	}
	m, err := s.runningMachine(ctx, box, user)
	if err != nil {
		return nil, err
	}
	op, started := s.ops.start(context.WithoutCancel(ctx), box, controlv1.Phase_PHASE_SAVING, m.Revision, stateTimeout)
	if started {
		go s.runRebase(op, user, m, image)
	}
	log.WithFunc("boxes.Service.Rebase").WithField("box", box).Infof(ctx, "rebasing from %s (%s) to %s as operation %s", m.ID, m.Image, image, op.id)
	return &controlv1.Operation{Id: op.id, BoxId: box}, nil
}

// runRebase saves quiesced, starts a machine from image, restores into it, then deletes the old machine.
func (s *Service) runRebase(op *operation, user string, old machines.Machine, image string) {
	logger := log.WithFunc("boxes.Service.runRebase").WithField("box", op.boxID)
	start := time.Now()
	rec, err := s.save(op, old, agentv1.Consistency_CONSISTENCY_QUIESCED)
	if err != nil {
		logger.Error(op.ctx, err, "save before rebase")
		op.fail("save failed")
		return
	}
	saved := time.Since(start)
	op.note(controlv1.Phase_PHASE_SAVING, controlv1.BoxState_BOX_STATE_RUNNING,
		fmt.Sprintf("saved snapshot %s in %s", rec.ID, saved.Round(time.Second)))

	spec := machines.Spec{ID: op.boxID + "-r" + strconv.FormatInt(time.Now().Unix(), 36), Owner: user, Flavor: old.Flavor, Image: image}
	created, _, err := s.machines.Ensure(op.ctx, spec)
	if err != nil {
		logger.Error(op.ctx, err, "create rebase machine")
		op.fail("create machine failed")
		return
	}
	op.note(controlv1.Phase_PHASE_CREATING, controlv1.BoxState_BOX_STATE_STARTING, fmt.Sprintf("machine %s from %s", created.ID, image))
	if err = s.waitRunning(op, created); err != nil {
		logger.Error(op.ctx, err, "start rebase machine")
		s.discard(op, created.ID)
		op.fail("new machine did not start")
		return
	}
	booted := time.Since(start)

	addr, err := s.machines.Address(op.ctx, created.ID)
	if err == nil {
		_, err = s.state.restore(op.ctx, op.boxID, addr, rec.ID, progressNotes(op, controlv1.Phase_PHASE_RESTORING, "restoring"))
	}
	if err != nil {
		logger.Error(op.ctx, err, "restore into rebase machine")
		s.discard(op, created.ID)
		op.fail("restore failed")
		return
	}
	restored := time.Since(start)

	if err := s.machines.Delete(op.ctx, old.ID); err != nil {
		logger.Error(op.ctx, err, "delete old machine")
		op.fail("state restored into the new machine, but removing the old machine failed")
		return
	}
	logger.Infof(op.ctx, "rebased %s to %s (%s): saved %s, booted %s, restored %s", old.ID, created.ID, image,
		saved.Round(time.Second), booted.Round(time.Second), restored.Round(time.Second))
	op.succeed(fmt.Sprintf("now on %s (%s); snapshot %s, %.1f MiB logical; save %s, boot %s, restore %s",
		created.ID, image, rec.ID, float64(rec.LogicalBytes)/mib, saved.Round(time.Second),
		(booted - saved).Round(time.Second), (restored - booted).Round(time.Second)))
}

// waitRunning follows m until it runs, fails, or op ends.
func (s *Service) waitRunning(op *operation, m machines.Machine) error {
	for cur := range s.machines.Watch(op.ctx, m.ID, m.Revision) {
		switch cur.State {
		case machines.StateRunning:
			return nil
		case machines.StateFailed:
			return fmt.Errorf("machine %s failed: %s", m.ID, cur.Reason)
		case machines.StateGone:
			return fmt.Errorf("machine %s was deleted", m.ID)
		}
		op.note(controlv1.Phase_PHASE_CREATING, boxStates[cur.State], cur.Detail)
	}
	return fmt.Errorf("machine %s did not start: %w", m.ID, op.ctx.Err())
}

// discard deletes a rebase machine that will not serve; the old machine keeps serving.
func (s *Service) discard(op *operation, id string) {
	if err := s.machines.Delete(context.WithoutCancel(op.ctx), id); err != nil {
		log.WithFunc("boxes.Service.discard").WithField("box", op.boxID).Error(op.ctx, err, "delete "+id)
	}
}
