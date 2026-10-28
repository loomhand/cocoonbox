package boxes

import (
	"context"
	"slices"

	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

// RecordFunc records the box an agent's Create added, before its machine starts.
type RecordFunc func(ctx context.Context, box string) error

type agentKey struct{}

// WithAgent marks ctx's Creates as an agent's: the box counts against the agent's caps, gets none of the owner's credentials, and record runs on it before its machine starts.
func WithAgent(ctx context.Context, record RecordFunc) context.Context {
	return context.WithValue(ctx, agentKey{}, record)
}

// AgentRecord returns the RecordFunc of an agent's ctx, nil for the owner's.
func AgentRecord(ctx context.Context) RecordFunc {
	record, _ := ctx.Value(agentKey{}).(RecordFunc)
	return record
}

// agentBox reports whether owner's box is one an agent created.
func (s *Service) agentBox(owner, box string) bool {
	return slices.ContainsFunc(s.registry.Boxes(owner), func(b users.Box) bool { return b.ID == box && b.Agent })
}

// ownerMachines drops the machines of agent boxes from ms.
func (s *Service) ownerMachines(ms []machines.Machine) []machines.Machine {
	return slices.DeleteFunc(slices.Clone(ms), func(m machines.Machine) bool { return s.agentBox(m.Owner, boxOfMachine(m.ID)) })
}
