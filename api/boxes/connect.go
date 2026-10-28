package boxes

import (
	"context"
	"errors"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/machines"
)

const wakePushTimeout = 30 * time.Second

// Connect issues each machine's box door key and the credentials its owner's grants put on its egress.
type Connect interface {
	DoorKey(ctx context.Context, machine, owner, incarnation string) (string, error)
	DropDoorKey(ctx context.Context, machine string) error
	PruneDoorKeys(ctx context.Context, live []string, cutoff time.Time) (int64, error)
	Credentials(ctx context.Context, owner string) (machines.Credentials, error)
}

// EnableConnect gives every machine its box door key and its owner's credentials.
func (s *Service) EnableConnect(c Connect) {
	s.connect = c
}

// ApplyCredentials brings every machine of user in line with the user's credentials; flags change only on running ones.
func (s *Service) ApplyCredentials(ctx context.Context, user string) error {
	if s.connect == nil {
		return nil
	}
	ms, err := s.machines.List(ctx, user)
	if err != nil {
		return err
	}
	creds, err := s.connect.Credentials(ctx, user)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range s.ownerMachines(ms) {
		errs = append(errs, s.machines.SetCredentials(ctx, m.ID, creds))
	}
	return errors.Join(errs...)
}

// MachineOrigin returns the box a machine serves and the image it runs; the image is empty when the backend cannot say.
func (s *Service) MachineOrigin(ctx context.Context, machine string) (string, string) {
	m, err := s.machines.Get(ctx, machine)
	if err != nil {
		return boxOfMachine(machine), ""
	}
	return boxOfMachine(machine), m.Image
}

// RunCredentials pushes the credentials of running machines every interval until ctx ends, so a token is refreshed before it expires.
func (s *Service) RunCredentials(ctx context.Context, interval time.Duration) {
	tick := time.Tick(interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		s.refreshCredentials(ctx)
	}
}

// PruneDoorKeys deletes the box door keys of machines the backend no longer holds; a hibernated machine keeps its key.
func (s *Service) PruneDoorKeys(ctx context.Context) error {
	if s.connect == nil {
		return nil
	}
	cutoff := time.Now()
	ms, err := s.machines.List(ctx, "")
	if err != nil {
		return err
	}
	logger := log.WithFunc("boxes.Service.PruneDoorKeys")
	if len(ms) == 0 {
		logger.Warn(ctx, "the backend lists no machine; keeping every door key")
		return nil
	}
	live := make([]string, 0, len(ms))
	for _, m := range ms {
		live = append(live, m.ID)
	}
	n, err := s.connect.PruneDoorKeys(ctx, live, cutoff)
	if err != nil {
		return err
	}
	logger.Infof(ctx, "pruned %d door keys of gone machines, kept those of %d machines", n, len(live))
	return nil
}

// refreshCredentials reads each owner's credentials once and applies them to the owner's running machines.
func (s *Service) refreshCredentials(ctx context.Context) {
	logger := log.WithFunc("boxes.Service.refreshCredentials")
	ms, err := s.machines.List(ctx, "")
	if err != nil {
		logger.Error(ctx, err, "list machines")
		return
	}
	running := map[string][]string{}
	for _, m := range s.ownerMachines(ms) {
		if m.State == machines.StateRunning {
			running[m.Owner] = append(running[m.Owner], m.ID)
		}
	}
	for owner, ids := range running {
		creds, err := s.connect.Credentials(ctx, owner)
		if err != nil {
			logger.Warnf(ctx, "read the credentials of %s: %v", owner, err)
			continue
		}
		for _, id := range ids {
			if err := s.machines.SetCredentials(ctx, id, creds); err != nil {
				logger.WithField("machine", id).Warnf(ctx, "push credentials: %v", err)
			}
		}
	}
}

// wakeCredentials pushes m's secrets and keys while it wakes, without holding up the wake; provisionOnce pushes again after it.
func (s *Service) wakeCredentials(ctx context.Context, m machines.Machine) {
	vault := s.vault != nil && s.provision != nil
	if s.connect == nil && !vault {
		return
	}
	if s.agentBox(m.Owner, boxOfMachine(m.ID)) {
		return
	}
	go func() {
		pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wakePushTimeout)
		defer cancel()
		if s.connect != nil {
			s.pushCredentials(pushCtx, m)
		}
		if vault {
			if err := s.syncVault(pushCtx, m.ID); err != nil {
				log.WithFunc("boxes.Service.wakeCredentials").WithField("machine", m.ID).Warnf(pushCtx, "push keys: %v", err)
			}
		}
	}()
}

// pushCredentials applies the credentials of m's owner to m; a failure is logged, and the next push retries.
func (s *Service) pushCredentials(ctx context.Context, m machines.Machine) {
	creds, err := s.connect.Credentials(ctx, m.Owner)
	if err == nil {
		err = s.machines.SetCredentials(ctx, m.ID, creds)
	}
	if err != nil {
		log.WithFunc("boxes.Service.pushCredentials").WithField("machine", m.ID).Warnf(ctx, "push credentials: %v", err)
	}
}

func (s *Service) deleteMachine(ctx context.Context, id string) error {
	err := s.machines.Delete(ctx, id)
	if s.connect != nil && (err == nil || errors.Is(err, machines.ErrNotFound)) {
		if dropErr := s.connect.DropDoorKey(ctx, id); dropErr != nil {
			log.WithFunc("boxes.Service.deleteMachine").WithField("machine", id).Warnf(ctx, "drop the box door key: %v", dropErr)
		}
	}
	return err
}
