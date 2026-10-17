package boxes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/metadata"

	"github.com/loomhand/cocoonbox/api/credential"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	metaWake = "wake"
	metaGW   = "gw"

	gwKeyEnv      = "GW_KEY"
	connectKeyEnv = "CB_CONNECT_KEY"

	provisionAttempts = 5
	provisionBackoff  = 2 * time.Second
)

// Signer issues the credentials box agents verify with the key named in their identity file.
type Signer interface {
	KeyID() string
	PublicKey() string
	Credential(box, wake string) string
}

// EnableProvisioning gives every machine an identity file for signer and, with accounts, its user's gw key.
func (s *Service) EnableProvisioning(signer Signer) {
	s.provision = &provisioner{signer: signer, backoff: provisionBackoff, locks: map[string]*sync.Mutex{}}
}

// RunProvisioner provisions running machines that came up without an operation, until ctx ends.
func (s *Service) RunProvisioner(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.provisionStale(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) provisionStale(ctx context.Context) {
	ms, err := s.machines.List(ctx, "")
	if err != nil {
		log.WithFunc("boxes.Service.provisionStale").Error(ctx, err, "list machines")
		return
	}
	for _, m := range ms {
		if err := s.syncExit(ctx, m); err != nil {
			log.WithFunc("boxes.Service.provisionStale").WithField("machine", m.ID).Error(ctx, err, "sync exit")
		}
		if m.State == machines.StateRunning && wakeOf(m) == "" {
			_ = s.provisionMachine(ctx, m)
		}
	}
	if s.vault != nil {
		s.reconcileVault(ctx, ms)
	}
}

// provisionMachine provisions m unless its current incarnation already has a wake id.
func (s *Service) provisionMachine(ctx context.Context, m machines.Machine) error {
	p := s.provision
	unlock := p.lock(m.ID)
	defer unlock()
	if s.userLocks != nil {
		unlockShared, err := s.userLocks.Lock(ctx, "provision:"+m.ID)
		if err != nil {
			return err
		}
		defer unlockShared()
		if s.syncMachines != nil {
			if err := s.syncMachines(ctx, m.Owner); err != nil {
				return err
			}
		}
	}
	cur, err := s.machines.Get(ctx, m.ID)
	if err != nil {
		return err
	}
	if wakeOf(cur) != "" {
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
		logger.Warnf(ctx, "provision attempt %d: %v", attempt+1, lastErr)
	}
	return lastErr
}

func (s *Service) provisionOnce(ctx context.Context, m machines.Machine) (string, error) {
	p := s.provision
	if err := s.syncExit(ctx, m); err != nil {
		return "", fmt.Errorf("exit: %w", err)
	}
	keys := map[string]*machines.Secret{}
	if s.accounts != nil && m.Meta[metaGW] == "" {
		key, err := s.accounts.GWKey(m.Owner)
		if err != nil {
			return "", fmt.Errorf("gw key: %w", err)
		}
		if key != "" {
			keys[gwKeyEnv] = &machines.Secret{Value: "Bearer " + key}
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("wake id: %w", err)
	}
	box := boxOfMachine(m.ID)
	identity := machines.Identity{BoxID: box, WakeID: hex.EncodeToString(raw), KeyID: p.signer.KeyID(), PublicKey: p.signer.PublicKey(), Name: s.boxName(m.Owner, box)}
	owned := !s.agentBox(m.Owner, box)
	if s.connect != nil && owned {
		key, err := s.connect.DoorKey(ctx, m.ID, m.Owner, m.Incarnation)
		if err != nil {
			log.WithFunc("boxes.Service.provisionOnce").WithField("machine", m.ID).Warnf(ctx, "issue the box door key: %v", err)
		}
		if key != "" {
			keys[connectKeyEnv] = &machines.Secret{Value: "Bearer " + key}
		}
	}
	if err := s.machines.Deliver(ctx, m.ID, identity, keys); err != nil {
		return "", err
	}
	if s.connect != nil && owned {
		s.pushCredentials(ctx, m)
	}
	if s.vault != nil && owned {
		if err := s.pushVault(ctx, m); err != nil {
			log.WithFunc("boxes.Service.provisionOnce").WithField("machine", m.ID).Warnf(ctx, "push keys: %v", err)
		}
	}
	if keys[gwKeyEnv] != nil {
		if err := s.machines.SetMeta(ctx, m.ID, metaGW, "1"); err != nil {
			return "", err
		}
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
	return m.State == machines.StateRunning && (s.provision == nil || wakeOf(m) != "") &&
		(active == nil || (active.phase != controlv1.Phase_PHASE_RESTORING && !active.park))
}

// provisioner writes each machine's identity file on every boot and its gw key on first boot.
type provisioner struct {
	signer  Signer
	backoff time.Duration

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (p *provisioner) lock(machine string) func() {
	p.mu.Lock()
	l := p.locks[machine]
	if l == nil {
		l = &sync.Mutex{}
		p.locks[machine] = l
	}
	p.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// wakeOf returns the wake id recorded for m's current incarnation, "" when none.
func wakeOf(m machines.Machine) string {
	incarnation, wake, ok := strings.Cut(m.Meta[metaWake], "|")
	if !ok || m.Incarnation == "" || incarnation != m.Incarnation {
		return ""
	}
	return wake
}
