package boxes

import (
	"context"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/dialer"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/store"
)

const (
	mib              = 1 << 20
	progressInterval = time.Second
)

// EnableState turns on saves and restores through agents into st, with per-user keys from keys.
func (s *Service) EnableState(agents StateAgents, st store.Store, keys keyring.Keyring) {
	s.state = newStateLayer(agents, st, keys)
	s.state.deltaFor = func(user string) bool { return s.registry.Policy(user).Delta }
	s.state.accountExists = s.registry.AccountExists
}

// EnableConditionalWrites makes each index and timeline rewrite land only on the version it read, so api processes on one store keep each other's writes.
func (s *Service) EnableConditionalWrites(v store.Versioned) {
	s.state.versioned = v
}

func (s *Service) SaveState(ctx context.Context, ref *controlv1.BoxRef) (*controlv1.Operation, error) {
	box := ref.GetBoxId()
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
	op, err := s.ops.startIfIdle(ctx, user, box, controlv1.Phase_PHASE_SAVING, m.Revision)
	if err != nil {
		return nil, err
	}
	go s.runSave(op, user, m, agentv1.Consistency_CONSISTENCY_QUIESCED)
	return &controlv1.Operation{Id: op.id, BoxId: box}, nil
}

// RunScheduler saves every running box whose latest snapshot is older than interval, until ctx ends.
func (s *Service) RunScheduler(ctx context.Context, interval time.Duration) {
	every(ctx, interval/4, func() { s.saveStale(ctx, interval) })
}

func (s *Service) saveStale(ctx context.Context, interval time.Duration) {
	logger := log.WithFunc("boxes.Service.saveStale")
	ms, err := s.machines.List(ctx, "")
	if err != nil {
		logger.Error(ctx, err, "list machines")
		return
	}
	for _, m := range ms {
		if !s.saveDue(ctx, m, interval) {
			continue
		}
		if err := s.state.scheduled.Acquire(ctx, 1); err != nil {
			return
		}
		s.scheduleSave(ctx, m, interval)
	}
}

func (s *Service) scheduleSave(ctx context.Context, m machines.Machine, interval time.Duration) {
	release := func() { s.state.scheduled.Release(1) }
	unlock, err := s.lockShared(ctx, m.Owner)
	if err != nil {
		release()
		return
	}
	defer unlock()
	box := boxOfMachine(m.ID)
	cur, err := s.current(ctx, m.Owner, box)
	if err != nil || cur.ID != m.ID || !s.saveDue(ctx, cur, interval) {
		release()
		return
	}
	op, err := s.ops.startIfIdle(ctx, cur.Owner, box, controlv1.Phase_PHASE_SAVING, cur.Revision)
	if err != nil {
		release()
		return
	}
	go func() {
		defer release()
		s.runSave(op, cur.Owner, cur, agentv1.Consistency_CONSISTENCY_LIVE)
	}()
	log.WithFunc("boxes.Service.scheduleSave").Infof(ctx, "scheduled live save of %s as operation %s", box, op.id)
}

func (s *Service) saveDue(ctx context.Context, m machines.Machine, interval time.Duration) bool {
	box := boxOfMachine(m.ID)
	p := statePath{user: m.Owner, box: box}
	return s.ready(m) && s.ops.activeFor(box) == nil &&
		time.Since(later(time.UnixMilli(s.state.lastSaved(ctx, p)), s.state.lastFailed(p))) >= interval
}

// runningMachine returns the serving machine of user's box when the state layer can use it now.
func (s *Service) runningMachine(ctx context.Context, box, user string) (machines.Machine, error) {
	if s.state == nil {
		return machines.Machine{}, status.Error(codes.FailedPrecondition, "state store is not configured")
	}
	m, err := s.current(ctx, user, box)
	if err != nil {
		return machines.Machine{}, serviceError(ctx, log.WithFunc("boxes.Service.runningMachine").WithField("box", box), err, "get box")
	}
	if !s.ready(m) {
		return machines.Machine{}, status.Errorf(codes.FailedPrecondition, "box is %s, not running", boxStates[m.State])
	}
	if active := s.ops.activeFor(box); active != nil {
		return machines.Machine{}, stillRunning(active.id)
	}
	return m, nil
}

func (s *Service) runSave(op *operation, user string, m machines.Machine, consistency agentv1.Consistency) {
	running := controlv1.BoxState_BOX_STATE_RUNNING
	rec, err := s.save(op, user, m, consistency)
	if err != nil {
		log.WithFunc("boxes.Service.runSave").WithField("box", op.boxID).Error(op.ctx, err, "save state")
		s.state.noteFailed(statePath{user: user, box: op.boxID})
		op.fail("save failed")
		return
	}
	op.note(controlv1.Phase_PHASE_SAVING, running, fmt.Sprintf("uploaded %d new chunks (%.1f MiB) of %d", rec.NewChunks, float64(rec.UploadedBytes)/mib, len(rec.Chunks)))
	op.succeed(fmt.Sprintf("snapshot %s: %d files, %.1f MiB logical, %.1f MiB uploaded", rec.ID, rec.Files, float64(rec.LogicalBytes)/mib, float64(rec.UploadedBytes)/mib))
}

func (s *Service) save(op *operation, user string, m machines.Machine, consistency agentv1.Consistency) (*snapshotRecord, error) {
	ep, err := s.machines.Address(op.ctx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("address of %s: %w", m.ID, err)
	}
	op.note(controlv1.Phase_PHASE_SAVING, controlv1.BoxState_BOX_STATE_RUNNING, fmt.Sprintf("planning a %s snapshot", consistencyName(consistency)))
	auth := func(ctx context.Context) context.Context { return s.agentContext(ctx, m) }
	return s.state.save(op.ctx, auth, statePath{user: user, box: op.boxID}, ep, m.Image, op.park, consistency, progressNotes(op, controlv1.Phase_PHASE_SAVING, "uploading"))
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

func stillRunning(id string) error {
	return status.Errorf(codes.FailedPrecondition, "operation %s is still running on the box; wait for it to end, then try again", id)
}
