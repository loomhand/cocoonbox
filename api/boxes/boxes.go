// Package boxes serves cocoonbox.control.v1.Boxes, one box per user, on any machines.Machines backend.
package boxes

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

// Service implements controlv1.BoxesServer and resolves boxes for the edge.
type Service struct {
	controlv1.UnimplementedBoxesServer

	machines machines.Machines
	flavor   machines.Flavor
	ops      *operations
}

// New serves boxes on backend, sizing new ones with defaultFlavor.
func New(backend machines.Machines, defaultFlavor machines.Flavor) *Service {
	return &Service{machines: backend, flavor: defaultFlavor, ops: newOperations()}
}

func (s *Service) Ensure(ctx context.Context, req *controlv1.EnsureRequest) (*controlv1.EnsureResponse, error) {
	user, err := callerUser(ctx)
	if err != nil {
		return nil, err
	}
	id := BoxID(user)
	flavor := resolveFlavor(req.GetFlavor(), s.flavor)
	logger := log.WithFunc("boxes.Service.Ensure").WithField("box", id)

	if req.GetFlavor() != nil {
		cur, getErr := s.machines.Get(ctx, id)
		if getErr == nil && cur.Flavor != flavor {
			return nil, status.Errorf(codes.FailedPrecondition, "box exists with flavor vcpus:%d memory_mib:%d disk_gib:%d",
				cur.Flavor.VCPUs, cur.Flavor.MemoryMiB, cur.Flavor.DiskGiB)
		}
		if getErr != nil && !errors.Is(getErr, machines.ErrNotFound) {
			return nil, serviceError(ctx, logger, getErr, "get box")
		}
	}
	m, action, err := s.machines.Ensure(ctx, machines.Spec{ID: id, Owner: user, Flavor: flavor})
	if err != nil {
		return nil, serviceError(ctx, logger, err, "ensure box")
	}
	switch action {
	case machines.ActionCreated:
		logger.Infof(ctx, "created box for user %s", user)
		return s.withOperation(ctx, m, controlv1.Phase_PHASE_CREATING), nil
	case machines.ActionWoken:
		logger.Info(ctx, "waking box")
		return s.withOperation(ctx, m, controlv1.Phase_PHASE_WAKING), nil
	}
	if m.State == machines.StateRunning {
		return &controlv1.EnsureResponse{Box: toBox(m)}, nil
	}
	phase := controlv1.Phase_PHASE_CREATING
	if cur := s.ops.activeFor(id); cur != nil && cur.phase == controlv1.Phase_PHASE_WAKING {
		phase = cur.phase
	}
	return s.withOperation(ctx, m, phase), nil
}

func (s *Service) Get(ctx context.Context, req *controlv1.GetBoxRequest) (*controlv1.Box, error) {
	id, err := ownedBox(ctx, req.GetBoxId())
	if err != nil {
		return nil, err
	}
	m, err := s.machines.Get(ctx, id)
	if err != nil {
		return nil, serviceError(ctx, log.WithFunc("boxes.Service.Get").WithField("box", id), err, "get box")
	}
	return toBox(m), nil
}

func (s *Service) List(ctx context.Context, _ *controlv1.ListBoxesRequest) (*controlv1.ListBoxesResponse, error) {
	user, err := callerUser(ctx)
	if err != nil {
		return nil, err
	}
	ms, err := s.machines.List(ctx, user)
	if err != nil {
		return nil, serviceError(ctx, log.WithFunc("boxes.Service.List").WithField("user", user), err, "list boxes")
	}
	resp := &controlv1.ListBoxesResponse{}
	for _, m := range ms {
		resp.Boxes = append(resp.Boxes, toBox(m))
	}
	return resp, nil
}

func (s *Service) Hibernate(ctx context.Context, ref *controlv1.BoxRef) (*controlv1.Operation, error) {
	id, err := ownedBox(ctx, ref.GetBoxId())
	if err != nil {
		return nil, err
	}
	logger := log.WithFunc("boxes.Service.Hibernate").WithField("box", id)
	m, err := s.machines.Hibernate(ctx, id)
	if err != nil {
		return nil, serviceError(ctx, logger, err, "hibernate box")
	}
	logger.Info(ctx, "hibernating box")
	return s.withOperation(ctx, m, controlv1.Phase_PHASE_SNAPSHOTTING).GetOperation(), nil
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

// Resolve returns the agent address of boxID after checking that user owns it and it runs.
func (s *Service) Resolve(ctx context.Context, user, boxID string) (string, error) {
	if boxID != BoxID(user) {
		return "", status.Error(codes.PermissionDenied, "box belongs to another user")
	}
	addr, err := s.machines.Address(ctx, boxID)
	var notRunning *machines.NotRunningError
	switch {
	case err == nil:
		return addr, nil
	case errors.Is(err, machines.ErrNotFound):
		return "", status.Error(codes.NotFound, "box not found")
	case errors.As(err, &notRunning):
		return "", status.Errorf(codes.Unavailable, "box is %s", boxStates[notRunning.State])
	case errors.Is(err, machines.ErrNoAddress):
		return "", status.Error(codes.Unavailable, "box has no address yet")
	default:
		log.WithFunc("boxes.Service.Resolve").WithField("box", boxID).Error(ctx, err, "resolve box")
		return "", status.Error(codes.Unavailable, "resolve box failed")
	}
}

func (s *Service) withOperation(ctx context.Context, m machines.Machine, phase controlv1.Phase) *controlv1.EnsureResponse {
	op, started := s.ops.start(context.WithoutCancel(ctx), m.ID, phase, m.Revision)
	if started {
		go s.follow(op)
	}
	return &controlv1.EnsureResponse{
		Box:       toBox(m),
		Operation: &controlv1.Operation{Id: op.id, BoxId: op.boxID},
	}
}

// follow records the backend's view of op's box until op finishes or times out.
func (s *Service) follow(op *operation) {
	for m := range s.machines.Watch(op.ctx, op.boxID, op.gen) {
		if m.State == machines.StateGone {
			op.fail("box was deleted")
			return
		}
		op.record(toObservation(m))
	}
	if errors.Is(op.ctx.Err(), context.DeadlineExceeded) {
		op.fail(fmt.Sprintf("timed out after %s", operationTimeout))
	}
}

func callerUser(ctx context.Context) (string, error) {
	if user := auth.User(ctx); user != "" {
		return user, nil
	}
	return "", status.Error(codes.Unauthenticated, "unauthenticated")
}

func ownedBox(ctx context.Context, boxID string) (string, error) {
	user, err := callerUser(ctx)
	if err != nil {
		return "", err
	}
	if boxID != BoxID(user) {
		return "", status.Error(codes.PermissionDenied, "box belongs to another user")
	}
	return boxID, nil
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
