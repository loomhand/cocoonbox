package boxes

import (
	"context"
	"errors"

	"github.com/loomhand/cocoonbox/api/machines"
)

// ResidentialFunc reports whether user's boxes leave through the residential exit.
type ResidentialFunc func(user string) bool

type exitPolicy struct {
	upstream    string
	residential ResidentialFunc
}

// EnableResidential sends the machines of users residential reports through the proxy at upstream, and the rest direct.
func (s *Service) EnableResidential(upstream string, residential ResidentialFunc) {
	s.exit = &exitPolicy{upstream: upstream, residential: residential}
}

// ApplyExit brings every machine of user in line with its residential switch, for new connections.
func (s *Service) ApplyExit(ctx context.Context, user string) error {
	ms, err := s.machines.List(ctx, user)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range ms {
		errs = append(errs, s.syncExit(ctx, m, false))
	}
	return errors.Join(errs...)
}

// syncExit points m's egress at the exit its owner's switch names when it differs or force asks.
func (s *Service) syncExit(ctx context.Context, m machines.Machine, force bool) error {
	if s.exit == nil {
		return nil
	}
	on := s.exit.residential(m.Owner)
	if on == m.Routed && !force {
		return nil
	}
	var upstream string
	if on {
		upstream = s.exit.upstream
	}
	return s.machines.SetEgress(ctx, m.ID, upstream)
}
