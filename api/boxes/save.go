package boxes

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/dialer"
	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/keyring"
	"github.com/CMGS/cocoonbox/api/machines"
	"github.com/CMGS/cocoonbox/api/store"
)

const (
	mib              = 1 << 20
	progressInterval = time.Second
)

// EnableState turns on saves and restores through agents into st, with per-box keys from keys.
func (s *Service) EnableState(agents StateAgents, st store.Store, keys keyring.Keyring) {
	s.state = newStateLayer(agents, st, keys)
}

func (s *Service) SaveState(ctx context.Context, ref *controlv1.BoxRef) (*controlv1.Operation, error) {
	box := ref.GetBoxId()
	user, err := ownerOf(ctx, box)
	if err != nil {
		return nil, err
	}
	m, err := s.runningMachine(ctx, box, user)
	if err != nil {
		return nil, err
	}
	op := s.startSave(ctx, box, m, agentv1.Consistency_CONSISTENCY_QUIESCED)
	return &controlv1.Operation{Id: op.id, BoxId: box}, nil
}

// RunScheduler saves every running box of users whose latest snapshot is older than interval, until ctx ends.
func (s *Service) RunScheduler(ctx context.Context, interval time.Duration, users []string) {
	ticker := time.NewTicker(interval / 4)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.saveStale(ctx, interval, users)
		}
	}
}

func (s *Service) saveStale(ctx context.Context, interval time.Duration, users []string) {
	logger := log.WithFunc("boxes.Service.saveStale")
	ms, err := s.machines.List(ctx, "")
	if err != nil {
		logger.Error(ctx, err, "list machines")
		return
	}
	for _, m := range ms {
		box := BoxID(m.Owner)
		if !slices.Contains(users, m.Owner) || !s.ready(m) || s.ops.activeFor(box) != nil {
			continue
		}
		if time.Since(time.UnixMilli(s.state.lastSaved(ctx, box))) < interval {
			continue
		}
		op := s.startSave(ctx, box, m, agentv1.Consistency_CONSISTENCY_LIVE)
		logger.Infof(ctx, "scheduled live save of %s as operation %s", box, op.id)
	}
}

// runningMachine returns the serving machine of user's box when the state layer can use it now.
func (s *Service) runningMachine(ctx context.Context, box, user string) (machines.Machine, error) {
	if s.state == nil {
		return machines.Machine{}, status.Error(codes.FailedPrecondition, "state store is not configured")
	}
	m, err := s.current(ctx, user)
	if err != nil {
		return machines.Machine{}, serviceError(ctx, log.WithFunc("boxes.Service.runningMachine").WithField("box", box), err, "get box")
	}
	if !s.ready(m) {
		return machines.Machine{}, status.Errorf(codes.FailedPrecondition, "box is %s, not running", boxStates[m.State])
	}
	if active := s.ops.activeFor(box); active != nil {
		return machines.Machine{}, status.Errorf(codes.FailedPrecondition, "operation %s is still running", active.id)
	}
	return m, nil
}

// startSave runs a save of box from machine m as an operation.
func (s *Service) startSave(ctx context.Context, box string, m machines.Machine, consistency agentv1.Consistency) *operation {
	op, started := s.ops.start(context.WithoutCancel(ctx), box, controlv1.Phase_PHASE_SAVING, m.Revision, stateTimeout)
	if started {
		go s.runSave(op, m, consistency)
	}
	return op
}

func (s *Service) runSave(op *operation, m machines.Machine, consistency agentv1.Consistency) {
	running := controlv1.BoxState_BOX_STATE_RUNNING
	rec, err := s.save(op, m, consistency)
	if err != nil {
		log.WithFunc("boxes.Service.runSave").WithField("box", op.boxID).Error(op.ctx, err, "save state")
		op.fail("save failed")
		return
	}
	op.note(controlv1.Phase_PHASE_SAVING, running, fmt.Sprintf("uploaded %d new chunks (%.1f MiB) of %d", rec.NewChunks, float64(rec.UploadedBytes)/mib, len(rec.Chunks)))
	op.succeed(fmt.Sprintf("snapshot %s: %d files, %.1f MiB logical, %.1f MiB uploaded", rec.ID, rec.Files, float64(rec.LogicalBytes)/mib, float64(rec.UploadedBytes)/mib))
}

func (s *Service) save(op *operation, m machines.Machine, consistency agentv1.Consistency) (*snapshotRecord, error) {
	ep, err := s.machines.Address(op.ctx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("address of %s: %w", m.ID, err)
	}
	op.note(controlv1.Phase_PHASE_SAVING, controlv1.BoxState_BOX_STATE_RUNNING, fmt.Sprintf("planning a %s snapshot", consistencyName(consistency)))
	return s.state.save(s.agentContext(op.ctx, m), op.boxID, ep, consistency, progressNotes(op, controlv1.Phase_PHASE_SAVING, "uploading"))
}

// progressNotes turns transfer progress into operation events, at most one per progressInterval.
func progressNotes(op *operation, phase controlv1.Phase, verb string) dialer.ProgressFunc {
	var last time.Time
	return func(p *agentv1.TransferProgress) {
		if !p.GetFinished() && time.Since(last) < progressInterval {
			return
		}
		last = time.Now()
		op.progress(phase, fmt.Sprintf("%s %d/%d chunks, %.1f/%.1f MiB", verb, p.GetDoneChunks(), p.GetTotalChunks(),
			float64(p.GetDoneBytes())/mib, float64(p.GetTotalBytes())/mib), p.GetDoneBytes(), p.GetTotalBytes())
	}
}

func consistencyName(c agentv1.Consistency) string {
	if c == agentv1.Consistency_CONSISTENCY_QUIESCED {
		return "quiesced"
	}
	return "live"
}
