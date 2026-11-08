package boxes

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	gib = 1 << 30
	// diskHeadroom is what first boot, caches and replayed packages add to a box's disk beside its saved files; the image is the overlay's lower layer, off the disk.
	diskHeadroom = 2 * gib
)

func (s *Service) Rebase(ctx context.Context, req *controlv1.RebaseRequest) (*controlv1.Operation, error) {
	box := req.GetBoxId()
	user, err := s.ownerOf(ctx, box)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lockShared(ctx, user)
	if err != nil {
		return nil, err
	}
	defer unlock()
	m, err := s.runningMachine(ctx, box, user)
	if err != nil {
		return nil, err
	}
	image := cmp.Or(req.GetImage(), s.defaults.upgrade(s.ownImage(user), m.Image))
	if image != s.defaults.Image && !slices.Contains(s.defaults.RebaseImages, image) {
		return nil, status.Errorf(codes.InvalidArgument, "image %q is not a rebase target; choose %s", image, s.defaults.choices())
	}
	if Family(image) != Family(m.Image) && !auth.Operator(ctx) {
		return nil, status.Errorf(codes.FailedPrecondition, "image %q is not of the box's family %s; only an operator moves a box to another family", image, Family(m.Image))
	}
	var resize *machines.Flavor
	if raw := auth.OperatorFlavor(ctx); raw != "" {
		flavor, err := parseFlavor(raw)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		resize = &flavor
	}
	op, started := s.ops.start(context.WithoutCancel(ctx), user, box, controlv1.Phase_PHASE_SAVING, m.Revision, stateTimeout)
	if !started {
		return nil, status.Errorf(codes.FailedPrecondition, "operation %s is still running", op.id)
	}
	go s.runRebase(op, user, m, image, resize)
	log.WithFunc("boxes.Service.Rebase").WithField("box", box).Infof(ctx, "rebasing from %s (%s) to %s as operation %s", m.ID, m.Image, image, op.id)
	return &controlv1.Operation{Id: op.id, BoxId: box}, nil
}

// runRebase saves quiesced, starts a machine from image, restores into it, then deletes the old machine; a resize flavor sizes the new machine and becomes the box's once it serves.
func (s *Service) runRebase(op *operation, user string, old machines.Machine, image string, resize *machines.Flavor) {
	logger := log.WithFunc("boxes.Service.runRebase").WithField("box", op.boxID)
	start := time.Now()
	rec, err := s.save(op, user, old, agentv1.Consistency_CONSISTENCY_QUIESCED)
	if err != nil {
		logger.Error(op.ctx, err, "save before rebase")
		op.fail("save failed")
		return
	}
	unpin, err := s.state.pin(op.ctx, statePath{user: user, box: op.boxID}, rec.ID)
	defer unpin()
	if err != nil {
		logger.Error(op.ctx, err, "pin the saved snapshot")
		op.fail("pin the saved snapshot failed")
		return
	}
	saved := time.Since(start)
	op.note(controlv1.Phase_PHASE_SAVING, controlv1.BoxState_BOX_STATE_RUNNING,
		fmt.Sprintf("saved snapshot %s in %s", rec.ID, saved.Round(time.Second)))

	tenant, err := s.accounts.Tenant(user)
	if err != nil {
		logger.Error(op.ctx, err, "read tenant for rebase machine")
		op.fail("create machine failed")
		return
	}
	flavor, ok := s.flavorOf(user, op.boxID)
	if !ok {
		op.fail("the box is gone")
		return
	}
	if resize != nil {
		if rec.LogicalBytes+diskHeadroom > uint64(resize.DiskGiB)*gib {
			op.fail(fmt.Sprintf("the box holds %.1f GiB, which a %d GiB disk does not take with room to boot", float64(rec.LogicalBytes)/gib, resize.DiskGiB))
			return
		}
		flavor = *resize
	}
	spec := machines.Spec{ID: newMachineID(op.boxID), Owner: user, Tenant: tenant, Flavor: flavor, Image: image}
	created, _, err := s.machines.Ensure(op.ctx, spec)
	if err != nil {
		logger.Error(op.ctx, err, "create rebase machine")
		op.fail("create machine failed")
		return
	}
	op.note(controlv1.Phase_PHASE_CREATING, controlv1.BoxState_BOX_STATE_STARTING, fmt.Sprintf("machine %s from %s", created.ID, image))
	running, err := s.waitReady(op, created)
	if err != nil {
		logger.Error(op.ctx, err, "start rebase machine")
		s.discard(op, created.ID)
		op.fail(startFailure(err))
		return
	}
	booted := time.Since(start)

	ep, err := s.machines.Address(op.ctx, created.ID)
	if err == nil {
		_, err = s.state.restore(s.agentContext(op.ctx, running), statePath{user: user, box: op.boxID}, ep, rec.ID, progressNotes(op, controlv1.Phase_PHASE_RESTORING, "restoring"))
	}
	if err != nil {
		logger.Error(op.ctx, err, "restore into rebase machine")
		s.discard(op, created.ID)
		op.fail("restore failed")
		return
	}
	restored := time.Since(start)

	unlock, err := s.lockShared(op.ctx, user)
	if err != nil {
		logger.Error(op.ctx, err, "lock the starts before removing the old machine")
		s.discard(op, created.ID)
		op.fail("rebase failed; the old machine stays")
		return
	}
	defer unlock()
	if s.ops.activeFor(op.boxID) != op || !s.ops.claim(op.ctx, op) {
		logger.Infof(op.ctx, "another operation took %s over; the old machine stays", op.boxID)
		s.discard(op, created.ID)
		op.fail("superseded; the old machine stays")
		return
	}
	if resize != nil {
		if err := s.registry.SetBoxFlavor(op.ctx, user, op.boxID, *resize); err != nil {
			logger.Error(op.ctx, err, "record the box's new flavor")
			s.discard(op, created.ID)
			op.fail("recording the new flavor failed; the old machine stays")
			return
		}
	}
	if err := s.deleteMachine(op.ctx, old.ID); err != nil {
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

// waitReady follows m until it runs, fails, or op ends, and returns it running and provisioned.
func (s *Service) waitReady(op *operation, m machines.Machine) (machines.Machine, error) {
	for cur := range s.machines.Watch(op.ctx, m.ID, m.Revision) {
		switch cur.State {
		case machines.StateRunning:
			if err := s.provisionMachine(op.ctx, cur, false); err != nil {
				return machines.Machine{}, err
			}
			return s.machines.Get(op.ctx, m.ID)
		case machines.StateFailed:
			if atCapacity(cur) {
				return machines.Machine{}, fmt.Errorf("machine %s: %w", m.ID, machines.ErrCapacity)
			}
			return machines.Machine{}, fmt.Errorf("machine %s failed: %s", m.ID, cur.Reason)
		case machines.StateGone:
			return machines.Machine{}, fmt.Errorf("machine %s was deleted", m.ID)
		}
		op.note(controlv1.Phase_PHASE_CREATING, boxStates[cur.State], stateWords[cur.State])
	}
	return machines.Machine{}, fmt.Errorf("machine %s did not start: %w", m.ID, op.ctx.Err())
}

// discard deletes machine id, which will not serve.
func (s *Service) discard(op *operation, id string) {
	if err := s.deleteMachine(context.WithoutCancel(op.ctx), id); err != nil {
		log.WithFunc("boxes.Service.discard").WithField("box", op.boxID).Error(op.ctx, err, "delete "+id)
	}
}

// parseFlavor reads a flavor written vcpus/memory_mib/disk_gib.
func parseFlavor(raw string) (machines.Flavor, error) {
	var f machines.Flavor
	var rest string
	if n, _ := fmt.Sscanf(raw, "%d/%d/%d%s", &f.VCPUs, &f.MemoryMiB, &f.DiskGiB, &rest); n != 3 || f.VCPUs == 0 || f.MemoryMiB == 0 || f.DiskGiB == 0 {
		return machines.Flavor{}, fmt.Errorf("flavor %q is not vcpus/memory_mib/disk_gib", raw)
	}
	return f, nil
}
