package boxes

import (
	"context"
	"errors"
	"maps"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

const (
	wakePushTimeout = 30 * time.Second
	// doorKeyAge is the age at which a running machine's door key is replaced.
	doorKeyAge = time.Hour
)

// Connect issues each machine's box door key and the credentials its owner's grants put on its egress.
type Connect interface {
	DoorKey(ctx context.Context, machine, owner, incarnation string) (string, error)
	DropDoorKey(ctx context.Context, machine string) error
	PruneDoorKeys(ctx context.Context, live []string, cutoff time.Time) (int64, error)
	StaleDoorKeys(ctx context.Context, age time.Duration) (map[string]string, error)
	RotateDoorKey(ctx context.Context, machine, owner, incarnation string, push func(key string) error) error
	Credentials(ctx context.Context, owner string) (machines.Credentials, error)
}

// EnableConnect gives every machine its box door key and its owner's credentials.
func (s *Service) EnableConnect(c Connect) {
	s.connect = c
}

// ApplyCredentials brings the running machines of user's own boxes in line with the user's credentials; a hibernated one gets them when it wakes.
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
		if m.State == machines.StateRunning {
			errs = append(errs, s.setCredentials(ctx, m, users.PushConnect, creds))
		}
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
	every(ctx, interval, func() { s.refreshCredentials(ctx) })
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

// refreshCredentials reads each owner's credentials once and applies them to the owner's running machines, with a new door key where the current one is older than doorKeyAge.
func (s *Service) refreshCredentials(ctx context.Context) {
	logger := log.WithFunc("boxes.Service.refreshCredentials")
	ms, err := s.machines.List(ctx, "")
	if err != nil {
		logger.Error(ctx, err, "list machines")
		return
	}
	var stale map[string]string
	if s.provision != nil {
		if stale, err = s.connect.StaleDoorKeys(ctx, doorKeyAge); err != nil {
			logger.Warnf(ctx, "list the stale door keys: %v", err)
		}
	}
	running := map[string][]machines.Machine{}
	for _, m := range s.ownerMachines(ms) {
		if m.State == machines.StateRunning {
			running[m.Owner] = append(running[m.Owner], m)
		}
	}
	for owner, owned := range running {
		creds, err := s.connect.Credentials(ctx, owner)
		if err != nil {
			logger.Warnf(ctx, "read the credentials of %s: %v", owner, err)
			continue
		}
		for _, m := range owned {
			push := s.setCredentials
			if incarnation, ok := stale[m.ID]; ok && incarnation == m.Incarnation {
				push = s.rotateDoorKey
			}
			if err := push(ctx, m, users.PushConnect, creds); err != nil {
				logger.WithField("machine", m.ID).Warnf(ctx, "push credentials: %v", err)
			}
		}
	}
}

// rotateDoorKey pushes creds to m with a new door key under m's provisioning locks, so no provisioning of another incarnation is between.
func (s *Service) rotateDoorKey(ctx context.Context, m machines.Machine, kind string, creds machines.Credentials) error {
	unlock := s.provision.locks.Lock(m.ID)
	defer unlock()
	if s.userLocks != nil {
		unlockShared, err := s.userLocks.LockProvision(ctx, "provision:"+m.ID)
		if err != nil {
			return err
		}
		defer unlockShared()
	}
	cur, err := s.machines.Get(ctx, m.ID)
	if err != nil || cur.State != machines.StateRunning || cur.Incarnation != m.Incarnation {
		return err
	}
	return s.connect.RotateDoorKey(ctx, m.ID, m.Owner, m.Incarnation, func(key string) error {
		withKey := creds
		withKey.Secrets = maps.Clone(creds.Secrets)
		withKey.Secrets[connectKeyEnv] = &machines.Secret{Value: "Bearer " + key}
		if err := s.recordPushes(ctx, m, users.PushDoor); err != nil {
			return err
		}
		return s.setCredentials(ctx, m, kind, withKey)
	})
}

// wakeCredentials pushes m's secrets and keys while it wakes, without holding up the wake; provisionOnce pushes again after it.
func (s *Service) wakeCredentials(ctx context.Context, m machines.Machine) {
	if s.agentBox(m.Owner, boxOfMachine(m.ID)) {
		return
	}
	go func() {
		pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wakePushTimeout)
		defer cancel()
		logger := log.WithFunc("boxes.Service.wakeCredentials").WithField("machine", m.ID)
		if s.connect != nil {
			if _, err := s.pushCredentials(pushCtx, m); err != nil {
				logger.Warnf(pushCtx, "push credentials: %v", err)
			}
		}
		if err := s.syncVault(pushCtx, m.ID); err != nil {
			logger.Warnf(pushCtx, "push keys: %v", err)
		}
	}()
}

// pushCredentials applies the credentials of m's owner to m and returns them.
func (s *Service) pushCredentials(ctx context.Context, m machines.Machine) (machines.Credentials, error) {
	creds, err := s.connect.Credentials(ctx, m.Owner)
	if err != nil {
		return creds, err
	}
	return creds, s.setCredentials(ctx, m, users.PushConnect, creds)
}

func (s *Service) deleteMachine(ctx context.Context, id string) error {
	err := s.machines.Delete(ctx, id)
	if err != nil && !errors.Is(err, machines.ErrNotFound) {
		return err
	}
	if s.connect != nil {
		if dropErr := s.connect.DropDoorKey(ctx, id); dropErr != nil {
			log.WithFunc("boxes.Service.deleteMachine").WithField("machine", id).Warnf(ctx, "drop the box door key: %v", dropErr)
		}
	}
	s.revokeMachineKeys(ctx, id)
	return err
}
