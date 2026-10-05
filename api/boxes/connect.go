package boxes

import (
	"context"
	"errors"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/CMGS/cocoonbox/api/machines"
)

const wakePushTimeout = 30 * time.Second

// Connect issues each machine's box door key and the credentials its owner's grants put on its egress.
type Connect interface {
	DoorKey(ctx context.Context, machine, owner, incarnation string) (string, error)
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
	for _, m := range ms {
		errs = append(errs, s.machines.SetCredentials(ctx, m.ID, creds))
	}
	return errors.Join(errs...)
}

// RunCredentials pushes the credentials of running machines every interval until ctx ends, so a token is refreshed
// and replaced before it expires.
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

// refreshCredentials reads each owner's credentials once and applies them to the owner's running machines.
func (s *Service) refreshCredentials(ctx context.Context) {
	logger := log.WithFunc("boxes.Service.refreshCredentials")
	ms, err := s.machines.List(ctx, "")
	if err != nil {
		logger.Error(ctx, err, "list machines")
		return
	}
	running := map[string][]string{}
	for _, m := range ms {
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

// wakeCredentials pushes m's secrets while it wakes, without holding up the wake; provisionOnce pushes again after it.
func (s *Service) wakeCredentials(ctx context.Context, m machines.Machine) {
	if s.connect == nil {
		return
	}
	go func() {
		pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wakePushTimeout)
		defer cancel()
		s.pushCredentials(pushCtx, m)
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
