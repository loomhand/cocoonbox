package boxes

import (
	"context"
	"errors"
	"fmt"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

// Accounts holds each user's backend tenant token and gw key.
type Accounts interface {
	Tenant(user string) (string, error)
	GWKey(user string) (string, error)
}

// EnableAccounts makes new machines claim with their user's tenant and carry their user's gw key.
func (s *Service) EnableAccounts(accounts Accounts) {
	s.accounts = accounts
}

// Release deletes every machine of user; the box's saved state stays in the store.
func (s *Service) Release(ctx context.Context, user string) error {
	ms, err := s.machines.List(ctx, user)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if err := s.deleteMachine(ctx, m.ID); err != nil && !errors.Is(err, machines.ErrNotFound) {
			return fmt.Errorf("delete machine %s: %w", m.ID, err)
		}
	}
	return nil
}

func (s *Service) HibernateAll(ctx context.Context, user string) error {
	current, err := s.currentMachines(ctx, user)
	if err != nil {
		return err
	}
	var errs []error
	for box, m := range current {
		if m.State != machines.StateRunning || !s.registry.HasBox(user, box) || s.parking(box) || s.deleting(box) {
			continue
		}
		if _, err := s.Hibernate(auth.WithUser(ctx, user), &controlv1.BoxRef{BoxId: box}); err != nil {
			errs = append(errs, fmt.Errorf("hibernate box %s: %w", box, err))
		}
	}
	return errors.Join(errs...)
}

// tenantOf returns user's tenant token, empty without accounts.
func (s *Service) tenantOf(user string) (string, error) {
	if s.accounts == nil {
		return "", nil
	}
	return s.accounts.Tenant(user)
}
