package boxes

import (
	"context"
	"slices"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

// RecordFunc records the box an agent's Create added, before its machine starts.
type RecordFunc func(ctx context.Context, box string) error

type StartCheck func(ctx context.Context) error

type agentKey struct{}

type startCheckKey struct{}

// WithAgent marks ctx's Creates as an agent's: the box counts against the agent's caps, gets none of the owner's credentials, and record runs on it before its machine starts.
func WithAgent(ctx context.Context, record RecordFunc) context.Context {
	return context.WithValue(ctx, agentKey{}, record)
}

// AgentRecord returns the RecordFunc of an agent's ctx, nil for the owner's.
func AgentRecord(ctx context.Context) RecordFunc {
	record, _ := ctx.Value(agentKey{}).(RecordFunc)
	return record
}

func WithStartCheck(ctx context.Context, check StartCheck) context.Context {
	return context.WithValue(ctx, startCheckKey{}, check)
}

func CheckStart(ctx context.Context) error {
	if check, ok := ctx.Value(startCheckKey{}).(StartCheck); ok {
		return check(ctx)
	}
	return nil
}

func (s *Service) admitStart(ctx context.Context, user string) (func(), error) {
	unlock, err := s.lockStarts(ctx, user)
	if err != nil {
		return nil, err
	}
	if err = CheckStart(ctx); err != nil {
		unlock()
		return nil, err
	}
	disabled, err := s.registry.AccountDisabled(ctx, user)
	switch {
	case err != nil:
		unlock()
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		log.WithFunc("boxes.Service.admitStart").WithField("user", user).Error(ctx, err, "read the account")
		return nil, status.Error(codes.Unavailable, "read the account failed")
	case disabled:
		unlock()
		return nil, status.Error(codes.FailedPrecondition, "the account is disabled")
	}
	return unlock, nil
}

// agentBox reports whether owner's box is one an agent created.
func (s *Service) agentBox(owner, box string) bool {
	return slices.ContainsFunc(s.registry.Boxes(owner), func(b users.Box) bool { return b.ID == box && b.Agent })
}

// ownerMachines drops the machines of agent boxes from ms.
func (s *Service) ownerMachines(ms []machines.Machine) []machines.Machine {
	return slices.DeleteFunc(slices.Clone(ms), func(m machines.Machine) bool { return s.agentBox(m.Owner, boxOfMachine(m.ID)) })
}
