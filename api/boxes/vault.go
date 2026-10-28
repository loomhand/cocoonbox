package boxes

import (
	"context"
	"errors"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/machines"
)

const metaVault = "vault"

// Vault gives the claim credentials of each owner's keys and a digest that changes with them.
type Vault interface {
	Digests(ctx context.Context) (map[string]string, error)
	Credentials(ctx context.Context, owner string) (machines.Credentials, string, error)
}

// EnableVault puts every owner's keys on the owner's machines; it needs provisioning.
func (s *Service) EnableVault(v Vault) {
	s.vault = v
}

// ApplyVault puts user's keys on every machine of user at once: host-only entries on all, the guest list on running ones.
func (s *Service) ApplyVault(ctx context.Context, user string) error {
	if s.vault == nil || s.provision == nil {
		return nil
	}
	ms, err := s.machines.List(ctx, user)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range s.ownerMachines(ms) {
		errs = append(errs, s.syncVault(ctx, m.ID))
	}
	return errors.Join(errs...)
}

// reconcileVault pushes the keys of every provisioned running machine whose recorded digest is not its owner's.
func (s *Service) reconcileVault(ctx context.Context, ms []machines.Machine) {
	logger := log.WithFunc("boxes.Service.reconcileVault")
	digests, err := s.vault.Digests(ctx)
	if err != nil {
		logger.Error(ctx, err, "read the key digests")
		return
	}
	for _, m := range s.ownerMachines(ms) {
		if m.State != machines.StateRunning || wakeOf(m) == "" || m.Meta[metaVault] == digests[m.Owner] {
			continue
		}
		if err := s.syncVault(ctx, m.ID); err != nil {
			logger.WithField("machine", m.ID).Warnf(ctx, "push keys: %v", err)
		}
	}
}

// syncVault pushes the keys of machine's owner under the machine's provisioning lock.
func (s *Service) syncVault(ctx context.Context, machine string) error {
	unlock := s.provision.locks.Lock(machine)
	defer unlock()
	m, err := s.machines.Get(ctx, machine)
	if err != nil {
		return err
	}
	return s.pushVault(ctx, m)
}

// pushVault applies the keys of m's owner to m and records their digest on a running m; the caller holds m's lock.
func (s *Service) pushVault(ctx context.Context, m machines.Machine) error {
	creds, digest, err := s.vault.Credentials(ctx, m.Owner)
	if err != nil {
		return err
	}
	if err := s.machines.SetCredentials(ctx, m.ID, creds); err != nil {
		return err
	}
	if m.State != machines.StateRunning || m.Meta[metaVault] == digest {
		return nil
	}
	return s.machines.SetMeta(ctx, m.ID, metaVault, digest)
}
