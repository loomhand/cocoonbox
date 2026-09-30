// Package boxes serves cocoonbox.control.v1.Boxes, one box per user, on any machines.Machines backend.
package boxes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

// Defaults size new boxes, name the image they start from, and list the other flavors Ensure and images Rebase accept.
type Defaults struct {
	Flavor       machines.Flavor
	Flavors      []machines.Flavor
	Image        string
	RebaseImages []string
}

// Service implements controlv1.BoxesServer and resolves boxes for the edge.
type Service struct {
	controlv1.UnimplementedBoxesServer

	machines  machines.Machines
	defaults  Defaults
	ops       *operations
	state     *stateLayer
	provision *provisioner
}

// New serves boxes on backend with defaults.
func New(backend machines.Machines, defaults Defaults) *Service {
	return &Service{machines: backend, defaults: defaults, ops: newOperations()}
}

func (s *Service) Ensure(ctx context.Context, req *controlv1.EnsureRequest) (*controlv1.EnsureResponse, error) {
	user, err := callerUser(ctx)
	if err != nil {
		return nil, err
	}
	box := BoxID(user)
	flavor := resolveFlavor(req.GetFlavor(), s.defaults.Flavor)
	if flavor != s.defaults.Flavor && !slices.Contains(s.defaults.Flavors, flavor) {
		return nil, status.Errorf(codes.InvalidArgument, "flavor vcpus:%d memory_mib:%d disk_gib:%d is not offered",
			flavor.VCPUs, flavor.MemoryMiB, flavor.DiskGiB)
	}
	logger := log.WithFunc("boxes.Service.Ensure").WithField("box", box)

	spec := machines.Spec{ID: box, Owner: user, Flavor: flavor}
	var saved head
	cur, err := s.current(ctx, user)
	switch {
	case err == nil:
		if active := s.ops.activeFor(box); active != nil && active.phase == controlv1.Phase_PHASE_RESTORING {
			return &controlv1.EnsureResponse{Box: s.toBox(ctx, box, cur), Operation: &controlv1.Operation{Id: active.id, BoxId: box}}, nil
		}
		if req.GetFlavor() != nil && cur.Flavor != flavor {
			return nil, status.Errorf(codes.FailedPrecondition, "box exists with flavor vcpus:%d memory_mib:%d disk_gib:%d",
				cur.Flavor.VCPUs, cur.Flavor.MemoryMiB, cur.Flavor.DiskGiB)
		}
		spec.ID = cur.ID
	case errors.Is(err, machines.ErrNotFound):
		if saved, err = s.savedHead(ctx, box); err != nil {
			return nil, serviceError(ctx, logger, err, "read saved state")
		}
	default:
		return nil, serviceError(ctx, logger, err, "get box")
	}
	m, action, err := s.machines.Ensure(ctx, spec)
	if err != nil {
		return nil, serviceError(ctx, logger, err, "ensure box")
	}
	switch action {
	case machines.ActionCreated:
		if saved.Latest != "" {
			logger.Infof(ctx, "box of user %s has no machine; restoring snapshot %s into %s", user, saved.Latest, m.ID)
			return s.restoreMissing(ctx, box, m, saved), nil
		}
		logger.Infof(ctx, "created box for user %s", user)
		return s.withOperation(ctx, box, m, controlv1.Phase_PHASE_CREATING), nil
	case machines.ActionWoken:
		logger.Info(ctx, "waking box")
		return s.withOperation(ctx, box, m, controlv1.Phase_PHASE_WAKING), nil
	}
	if s.ready(m) {
		return &controlv1.EnsureResponse{Box: s.toBox(ctx, box, m)}, nil
	}
	phase := controlv1.Phase_PHASE_CREATING
	if active := s.ops.activeFor(box); active != nil && active.phase == controlv1.Phase_PHASE_WAKING {
		phase = active.phase
	}
	return s.withOperation(ctx, box, m, phase), nil
}

func (s *Service) Get(ctx context.Context, req *controlv1.GetBoxRequest) (*controlv1.Box, error) {
	user, err := ownerOf(ctx, req.GetBoxId())
	if err != nil {
		return nil, err
	}
	m, err := s.current(ctx, user)
	if err != nil {
		return nil, serviceError(ctx, log.WithFunc("boxes.Service.Get").WithField("box", req.GetBoxId()), err, "get box")
	}
	return s.toBox(ctx, req.GetBoxId(), m), nil
}

func (s *Service) List(ctx context.Context, _ *controlv1.ListBoxesRequest) (*controlv1.ListBoxesResponse, error) {
	user, err := callerUser(ctx)
	if err != nil {
		return nil, err
	}
	m, err := s.current(ctx, user)
	if errors.Is(err, machines.ErrNotFound) {
		return &controlv1.ListBoxesResponse{}, nil
	}
	if err != nil {
		return nil, serviceError(ctx, log.WithFunc("boxes.Service.List").WithField("user", user), err, "list boxes")
	}
	return &controlv1.ListBoxesResponse{Boxes: []*controlv1.Box{s.toBox(ctx, BoxID(user), m)}}, nil
}

func (s *Service) Hibernate(ctx context.Context, ref *controlv1.BoxRef) (*controlv1.Operation, error) {
	box := ref.GetBoxId()
	user, err := ownerOf(ctx, box)
	if err != nil {
		return nil, err
	}
	logger := log.WithFunc("boxes.Service.Hibernate").WithField("box", box)
	cur, err := s.current(ctx, user)
	if err != nil {
		return nil, serviceError(ctx, logger, err, "get box")
	}
	if s.restoring(box) {
		return nil, status.Error(codes.FailedPrecondition, "box is being restored from its saved state")
	}
	m, err := s.machines.Hibernate(ctx, cur.ID)
	if err != nil {
		return nil, serviceError(ctx, logger, err, "hibernate box")
	}
	logger.Info(ctx, "hibernating box")
	return s.withOperation(ctx, box, m, controlv1.Phase_PHASE_SNAPSHOTTING).GetOperation(), nil
}

func (s *Service) WatchOperation(req *controlv1.WatchOperationRequest, stream grpc.ServerStreamingServer[controlv1.OperationEvent]) error {
	ctx := stream.Context()
	user, err := callerUser(ctx)
	if err != nil {
		return err
	}
	op := s.ops.get(req.GetOperationId())
	if op == nil || op.boxID != BoxID(user) {
		return status.Error(codes.NotFound, "operation not found")
	}
	offset := 0
	if key := req.GetFromOffsetKey(); key != "" {
		if offset, err = strconv.Atoi(key); err != nil || offset < 0 {
			return status.Errorf(codes.InvalidArgument, "invalid offset key %q", key)
		}
	}
	for {
		events, done, changed := op.since(offset)
		for _, ev := range events {
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
		offset += len(events)
		if done {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

// Resolve returns the agent endpoint of boxID and its wake id after checking that user owns it and it is ready.
func (s *Service) Resolve(ctx context.Context, user, boxID string) (machines.Endpoint, string, error) {
	var none machines.Endpoint
	if boxID != BoxID(user) {
		return none, "", status.Error(codes.PermissionDenied, "box belongs to another user")
	}
	cur, err := s.current(ctx, user)
	var ep machines.Endpoint
	if err == nil && !s.ready(cur) && cur.State == machines.StateRunning {
		err = &machines.NotRunningError{State: machines.StateStarting}
	}
	if err == nil {
		ep, err = s.machines.Address(ctx, cur.ID)
	}
	var notRunning *machines.NotRunningError
	switch {
	case err == nil:
		return ep, wakeOf(cur), nil
	case errors.Is(err, machines.ErrNotFound):
		return none, "", status.Error(codes.NotFound, "box not found")
	case errors.As(err, &notRunning):
		return none, "", status.Errorf(codes.Unavailable, "box is %s", boxStates[notRunning.State])
	case errors.Is(err, machines.ErrNoAddress):
		return none, "", status.Error(codes.Unavailable, "box has no address yet")
	default:
		log.WithFunc("boxes.Service.Resolve").WithField("box", boxID).Error(ctx, err, "resolve box")
		return none, "", status.Error(codes.Unavailable, "resolve box failed")
	}
}

// current returns user's serving machine: the oldest, which a rebase replaces only when it deletes it.
func (s *Service) current(ctx context.Context, user string) (machines.Machine, error) {
	ms, err := s.machines.List(ctx, user)
	if err != nil {
		return machines.Machine{}, err
	}
	if len(ms) == 0 {
		return machines.Machine{}, machines.ErrNotFound
	}
	return slices.MinFunc(ms, func(a, b machines.Machine) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	}), nil
}

func (s *Service) withOperation(ctx context.Context, box string, m machines.Machine, phase controlv1.Phase) *controlv1.EnsureResponse {
	op, started := s.ops.start(context.WithoutCancel(ctx), box, phase, m.Revision, operationTimeout)
	if started {
		go s.follow(op, m.ID)
	}
	return &controlv1.EnsureResponse{
		Box:       s.toBox(ctx, box, m),
		Operation: &controlv1.Operation{Id: op.id, BoxId: op.boxID},
	}
}

// follow records the backend's view of machine until op finishes or times out.
func (s *Service) follow(op *operation, machine string) {
	for m := range s.machines.Watch(op.ctx, machine, op.gen) {
		if m.State == machines.StateGone {
			op.fail("box was deleted")
			return
		}
		if m.State == machines.StateFailed {
			log.WithFunc("boxes.Service.follow").WithField("box", op.boxID).Warnf(op.ctx, "machine %s failed: %s (%s)", machine, m.Reason, m.Detail)
		}
		if m.State == machines.StateRunning && !s.ready(m) {
			op.record(observation{state: controlv1.BoxState_BOX_STATE_STARTING, detail: "provisioning"})
			if err := s.provisionMachine(op.ctx, m); err != nil {
				log.WithFunc("boxes.Service.follow").WithField("box", op.boxID).Error(op.ctx, err, "provision "+machine)
				op.fail(cmp.Or(failureWords[op.phase], "machine failed"))
				return
			}
			continue
		}
		op.record(toObservation(m, op.phase))
	}
	if errors.Is(op.ctx.Err(), context.DeadlineExceeded) {
		op.fail(fmt.Sprintf("timed out after %s", operationTimeout))
	}
}

func (s *Service) toBox(ctx context.Context, box string, m machines.Machine) *controlv1.Box {
	b := toBox(box, m)
	if m.State == machines.StateRunning && !s.ready(m) {
		b.State = controlv1.BoxState_BOX_STATE_STARTING
	}
	b.LatestImage = s.defaults.Image
	if s.state != nil {
		b.LastSavedAtMs = s.state.lastSaved(ctx, box)
	}
	return b
}

func callerUser(ctx context.Context) (string, error) {
	if user := auth.User(ctx); user != "" {
		return user, nil
	}
	return "", status.Error(codes.Unauthenticated, "unauthenticated")
}

// ownerOf returns the caller when it owns boxID.
func ownerOf(ctx context.Context, boxID string) (string, error) {
	user, err := callerUser(ctx)
	if err != nil {
		return "", err
	}
	if boxID != BoxID(user) {
		return "", status.Error(codes.PermissionDenied, "box belongs to another user")
	}
	return user, nil
}

func serviceError(ctx context.Context, logger *log.Fields, err error, action string) error {
	switch {
	case errors.Is(err, machines.ErrNotFound):
		return status.Error(codes.NotFound, "box not found")
	case errors.Is(err, machines.ErrDeleting):
		return status.Error(codes.FailedPrecondition, "box is being deleted")
	case errors.Is(err, machines.ErrConflict):
		logger.Error(ctx, err, action)
		return status.Errorf(codes.Aborted, "%s: concurrent change, retry", action)
	default:
		logger.Error(ctx, err, action)
		return status.Errorf(codes.Unavailable, "%s failed", action)
	}
}
