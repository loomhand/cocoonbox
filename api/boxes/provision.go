package boxes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/metadata"

	"github.com/loomhand/cocoonbox/api/credential"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/utils"
)

const (
	metaWake = "wake"
	metaGW   = "gw"

	gwKeyEnv      = "GW_KEY"
	connectKeyEnv = "CB_CONNECT_KEY"

	provisionAttempts = 5
	provisionBackoff  = 2 * time.Second
	provisionFanout   = 8
)

var errEnvMissing = errors.New("host-only entries stay missing after the push")

// Signer issues the credentials box agents verify with the key named in their identity file.
type Signer interface {
	KeyID() string
	PublicKey() string
	Credential(box, wake string) string
}

// provisioner writes each machine's identity file on every boot and its host-only keys whenever the backend lost them.
type provisioner struct {
	signer  Signer
	backoff time.Duration

	locks utils.Locks
}

// EnableProvisioning gives every machine an identity file for signer and, with accounts, its user's gw key.
func (s *Service) EnableProvisioning(signer Signer) {
	s.provision = &provisioner{signer: signer, backoff: provisionBackoff}
}

// RunProvisioner provisions running machines that came up without an operation, until ctx ends.
func (s *Service) RunProvisioner(ctx context.Context, interval time.Duration) {
	every(ctx, interval, func() { s.provisionStale(ctx) })
}

func (s *Service) provisionStale(ctx context.Context) {
	logger := log.WithFunc("boxes.Service.provisionStale")
	ms, err := s.machines.List(ctx, "")
	if err != nil {
		logger.Error(ctx, err, "list machines")
		return
	}
	current := serving(ms)
	var g errgroup.Group
	g.SetLimit(provisionFanout)
	for _, m := range ms {
		if current[boxOfMachine(m.ID)].ID != m.ID {
			s.dropStray(ctx, m)
			continue
		}
		if err := s.syncExit(ctx, m, false); err != nil {
			logger.WithField("machine", m.ID).Error(ctx, err, "sync exit")
		}
		if m.State == machines.StateRunning && (wakeOf(m) == "" || m.EnvMissing) {
			g.Go(func() error {
				if err := s.provisionMachine(ctx, m); err != nil {
					logger.WithField("machine", m.ID).Warnf(ctx, "provision: %v", err)
				}
				return nil
			})
		}
	}
	_ = g.Wait()
	if s.vault != nil {
		s.reconcileVault(ctx, ms)
	}
}

func (s *Service) dropStray(ctx context.Context, m machines.Machine) {
	box := boxOfMachine(m.ID)
	unlock, lockErr := s.lockStarts(ctx, m.Owner)
	if lockErr != nil {
		return
	}
	defer unlock()
	if s.ops.activeFor(box) != nil {
		return
	}
	if cur, err := s.current(ctx, m.Owner, box); err != nil || cur.ID == m.ID {
		return
	}
	logger := log.WithFunc("boxes.Service.dropStray").WithField("box", box)
	logger.Warnf(ctx, "machine %s serves no client and no operation runs on the box; deleting it", m.ID)
	if err := s.deleteMachine(ctx, m.ID); err != nil && !errors.Is(err, machines.ErrNotFound) {
		logger.Error(ctx, err, "delete "+m.ID)
	}
}

// provisionMachine provisions m unless its current incarnation already has a wake id and the backend holds all its host-only entries.
func (s *Service) provisionMachine(ctx context.Context, m machines.Machine) error {
	p := s.provision
	unlock := p.locks.Lock(m.ID)
	defer unlock()
	if s.userLocks != nil {
		unlockShared, err := s.userLocks.LockLong(ctx, "provision:"+m.ID)
		if err != nil {
			return err
		}
		defer unlockShared()
	}
	cur, err := s.machines.Get(ctx, m.ID)
	if err != nil {
		return err
	}
	if wakeOf(cur) != "" && !cur.EnvMissing {
		return nil
	}
	logger := log.WithFunc("boxes.Service.provisionMachine").WithField("machine", m.ID)
	var lastErr error
	for attempt := range provisionAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(p.backoff):
			}
		}
		var wake string
		if wake, lastErr = s.provisionOnce(ctx, cur); lastErr == nil {
			logger.Infof(ctx, "provisioned incarnation %s as wake %s", cur.Incarnation, wake)
			return nil
		}
		if errors.Is(lastErr, errEnvMissing) {
			cur.EnvMissing = true
		}
		logger.Warnf(ctx, "provision attempt %d: %v", attempt+1, lastErr)
	}
	return lastErr
}

// provisionOnce writes m's host-only keys and, unless the incarnation has a wake id, its identity; it fails while the backend still misses an entry.
func (s *Service) provisionOnce(ctx context.Context, m machines.Machine) (string, error) {
	logger := log.WithFunc("boxes.Service.provisionOnce").WithField("machine", m.ID)
	p := s.provision
	if err := s.syncExit(ctx, m, m.EnvMissing); err != nil {
		return "", fmt.Errorf("exit: %w", err)
	}
	wake := wakeOf(m)
	keys := map[string]*machines.Secret{}
	if s.accounts != nil && (m.Meta[metaGW] == "" || m.EnvMissing) {
		key, err := s.accounts.GWKey(m.Owner)
		if err != nil {
			return "", fmt.Errorf("gw key: %w", err)
		}
		if key != "" {
			keys[gwKeyEnv] = &machines.Secret{Value: "Bearer " + key}
		}
	}
	box := boxOfMachine(m.ID)
	owned := !s.agentBox(m.Owner, box)
	if s.connect != nil && owned {
		key, err := s.connect.DoorKey(ctx, m.ID, m.Owner, m.Incarnation)
		if err != nil {
			logger.Warnf(ctx, "issue the box door key: %v", err)
		}
		if key != "" {
			keys[connectKeyEnv] = &machines.Secret{Value: "Bearer " + key}
		}
	}
	var identity machines.Identity
	if wake == "" {
		identity = machines.Identity{BoxID: box, WakeID: utils.RandomHex(16), KeyID: p.signer.KeyID(), PublicKey: p.signer.PublicKey(), Name: s.boxName(m.Owner, box)}
		if err := s.machines.Deliver(ctx, m.ID, identity, keys); err != nil {
			return "", err
		}
	} else if err := s.machines.SetCredentials(ctx, m.ID, machines.Credentials{Secrets: keys}); err != nil {
		return "", err
	}
	if s.connect != nil && owned {
		s.pushCredentials(ctx, m)
	}
	if s.vault != nil && owned {
		if err := s.pushVault(ctx, m); err != nil {
			logger.Warnf(ctx, "push keys: %v", err)
		}
	}
	if cur, err := s.machines.Get(ctx, m.ID); err != nil || cur.EnvMissing {
		return "", cmp.Or(err, errEnvMissing)
	}
	if keys[gwKeyEnv] != nil && m.Meta[metaGW] == "" {
		if err := s.machines.SetMeta(ctx, m.ID, metaGW, "1"); err != nil {
			return "", err
		}
	}
	if wake != "" {
		return wake, nil
	}
	if err := s.machines.SetMeta(ctx, m.ID, metaWake, m.Incarnation+"|"+identity.WakeID); err != nil {
		return "", err
	}
	return identity.WakeID, nil
}

// agentContext carries m's credential to its agent on calls the control plane makes itself.
func (s *Service) agentContext(ctx context.Context, m machines.Machine) context.Context {
	wake := wakeOf(m)
	if s.provision == nil || wake == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, credential.Header, s.provision.signer.Credential(boxOfMachine(m.ID), wake))
}

// ready reports whether clients may reach m: running, not being restored or parked and, with provisioning on, provisioned for this incarnation.
func (s *Service) ready(m machines.Machine) bool {
	active := s.ops.activeFor(boxOfMachine(m.ID))
	return m.State == machines.StateRunning && (s.provision == nil || wakeOf(m) != "" && !m.EnvMissing) && !unrestored(m) &&
		(active == nil || (active.phase != controlv1.Phase_PHASE_RESTORING && !active.park))
}

// wakeOf returns the wake id recorded for m's current incarnation, "" when none.
func wakeOf(m machines.Machine) string {
	incarnation, wake, ok := strings.Cut(m.Meta[metaWake], "|")
	if !ok || m.Incarnation == "" || incarnation != m.Incarnation {
		return ""
	}
	return wake
}
