package boxes

import (
	"context"
	"errors"
	"fmt"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

// Accounts holds each user's backend tenant token and the gw key of each machine incarnation.
type Accounts interface {
	Tenant(user string) (string, error)
	IssueMachineKey(ctx context.Context, owner, machine, incarnation string) (string, string, error)
	MachineKeys(ctx context.Context, machine string) ([]users.MachineKey, error)
	RevokeMachineKey(ctx context.Context, id string) error
	InstallMachineKey(ctx context.Context, machine, id string) error
	RecordPushes(ctx context.Context, machine, owner, node string, kinds []string) error
}

// EnableAccounts makes new machines claim with their user's tenant and gives each running incarnation its own gw key.
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
		if m.State != machines.StateRunning && !m.HibernateFailed() || !s.registry.HasBox(user, box) || s.parking(box) || s.deleting(box) {
			continue
		}
		if _, err := s.Hibernate(auth.WithUser(ctx, user), &controlv1.BoxRef{BoxId: box}); err != nil {
			errs = append(errs, fmt.Errorf("hibernate box %s: %w", box, err))
		}
	}
	return errors.Join(errs...)
}

// setCredentials records a push of kind to m's node when creds carry a secret, then applies creds to m there only.
func (s *Service) setCredentials(ctx context.Context, m machines.Machine, kind string, creds machines.Credentials) error {
	if len(secretNames(creds.Secrets)) > 0 {
		if err := s.recordPushes(ctx, m, kind); err != nil {
			return err
		}
	}
	creds.Node = m.Node
	return s.machines.SetCredentials(ctx, m.ID, creds)
}

// recordPushes records that secrets of kinds go to m before they do.
func (s *Service) recordPushes(ctx context.Context, m machines.Machine, kinds ...string) error {
	if len(kinds) == 0 {
		return nil
	}
	if err := s.accounts.RecordPushes(ctx, m.ID, m.Owner, m.Node, kinds); err != nil {
		return fmt.Errorf("record pushes: %w", err)
	}
	return nil
}

// secretNames lists the names that secrets sets to a value.
func secretNames(secrets map[string]*machines.Secret) []string {
	var names []string
	for name, sec := range secrets {
		if sec != nil {
			names = append(names, name)
		}
	}
	return names
}
