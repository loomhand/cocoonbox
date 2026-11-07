package boxes

import (
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
	"github.com/loomhand/cocoonbox/api/users"
	"github.com/loomhand/cocoonbox/api/utils"
)

const (
	metaWake = "wake"

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

// EnableProvisioning gives every machine an identity file for signer and a gw key of its incarnation.
func (s *Service) EnableProvisioning(signer Signer) {
	s.provision = &provisioner{signer: signer, backoff: provisionBackoff}
}

// RunProvisioner provisions running machines that came up without an operation, until ctx ends.
func (s *Service) RunProvisioner(ctx context.Context, interval time.Duration) {
	every(ctx, interval, func() { s.provisionStale(ctx) })
}

func (s *Service) provisionStale(ctx context.Context) {
	logger := log.WithFunc("boxes.Service.provisionStale")
	// The keys are read before the machines, so a key issued after the listing never counts as ended.
	keys, err := s.accounts.MachineKeys(ctx, "")
	if err != nil {
		logger.Error(ctx, err, "list machine keys")
		return
	}
	ms, err := s.machines.List(ctx, "")
	if err != nil {
		logger.Error(ctx, err, "list machines")
		return
	}
	keyed, ended := endedKeys(ms, keys)
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
		rekey := wakeOf(m) != "" && !keyed[m.ID]
		if m.State == machines.StateRunning && (wakeOf(m) == "" || m.EnvMissing || rekey) {
			g.Go(func() error {
				if err := s.provisionMachine(ctx, m, rekey); err != nil {
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
	for _, k := range ended {
		s.revokeMachineKey(ctx, k.Machine, k.ID)
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

// provisionMachine provisions m unless its current incarnation has a wake id, the backend holds all its host-only entries and rekey is off.
func (s *Service) provisionMachine(ctx context.Context, m machines.Machine, rekey bool) error {
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
	if cur.State != machines.StateRunning {
		return &machines.NotRunningError{State: cur.State}
	}
	if wakeOf(cur) != "" && !cur.EnvMissing && !rekey {
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

// provisionOnce issues m's gw key and provisions m with it; a failure revokes the key or, once the incarnation has a wake id, every key of m, so the next pass keys it again.
func (s *Service) provisionOnce(ctx context.Context, m machines.Machine) (string, error) {
	keys := map[string]*machines.Secret{}
	issued, keyOnly := "", wakeOf(m) != "" && !m.EnvMissing
	key, id, err := s.accounts.IssueMachineKey(ctx, m.Owner, m.ID, m.Incarnation)
	switch {
	case err != nil && keyOnly:
		return "", fmt.Errorf("issue gw key: %w", err)
	case err != nil:
		log.WithFunc("boxes.Service.provisionOnce").WithField("machine", m.ID).Warnf(ctx, "issue the gw key; the next pass retries: %v", err)
		if installErr := s.accounts.InstallMachineKey(ctx, m.ID, ""); installErr != nil {
			return "", fmt.Errorf("record that no gw key is installed: %w", installErr)
		}
		keys[gwKeyEnv] = nil
	default:
		keys[gwKeyEnv], issued = &machines.Secret{Value: "Bearer " + key}, id
	}
	wake, err := s.provisionWith(ctx, m, keys)
	if err == nil && issued != "" {
		if installErr := s.accounts.InstallMachineKey(ctx, m.ID, issued); installErr != nil {
			log.WithFunc("boxes.Service.provisionOnce").WithField("machine", m.ID).Warnf(ctx, "record the installed gw key; the next pass keys the machine again: %v", installErr)
		}
	}
	switch {
	case err == nil || issued == "":
	case wakeOf(m) != "":
		s.revokeMachineKeys(context.WithoutCancel(ctx), m.ID)
	default:
		s.revokeMachineKey(context.WithoutCancel(ctx), m.ID, issued)
	}
	return wake, err
}

// provisionWith writes keys and m's other host-only entries and, unless the incarnation has a wake id, its identity; once every push succeeded it removes the entries the backend still lacks.
func (s *Service) provisionWith(ctx context.Context, m machines.Machine, keys map[string]*machines.Secret) (string, error) {
	logger := log.WithFunc("boxes.Service.provisionWith").WithField("machine", m.ID)
	p := s.provision
	if err := s.syncExit(ctx, m, m.EnvMissing); err != nil {
		return "", fmt.Errorf("sync exit: %w", err)
	}
	wake := wakeOf(m)
	box := boxOfMachine(m.ID)
	owned := !s.agentBox(m.Owner, box)
	var pushErr error
	if s.connect != nil && owned {
		key, err := s.connect.DoorKey(ctx, m.ID, m.Owner, m.Incarnation)
		if err != nil {
			pushErr = fmt.Errorf("issue the box door key: %w", err)
		}
		if key != "" {
			keys[connectKeyEnv] = &machines.Secret{Value: "Bearer " + key}
		}
	}
	var kinds []string
	if keys[gwKeyEnv] != nil {
		kinds = append(kinds, users.PushModel)
	}
	if keys[connectKeyEnv] != nil {
		kinds = append(kinds, users.PushDoor)
	}
	if err := s.recordPushes(ctx, m, kinds...); err != nil {
		return "", err
	}
	if err := s.machines.SetCredentials(ctx, m.ID, machines.Credentials{Secrets: keys, Node: m.Node}); err != nil {
		return "", err
	}
	var identity machines.Identity
	if wake == "" {
		identity = machines.Identity{BoxID: box, WakeID: utils.RandomHex(16), KeyID: p.signer.KeyID(), PublicKey: p.signer.PublicKey(), Name: s.boxName(m.Owner, box)}
		if err := s.machines.Deliver(ctx, m.ID, identity); err != nil {
			return "", err
		}
	}
	keep := secretNames(keys)
	if s.connect != nil && owned {
		creds, err := s.pushCredentials(ctx, m)
		keep, pushErr = append(keep, secretNames(creds.Secrets)...), errors.Join(pushErr, err)
	}
	if s.vault != nil && owned {
		creds, err := s.pushVault(ctx, m)
		keep, pushErr = append(keep, secretNames(creds.Secrets)...), errors.Join(pushErr, err)
	}
	cur, err := s.machines.Get(ctx, m.ID)
	switch {
	case err != nil:
		return "", err
	case cur.EnvMissing && pushErr != nil:
		return "", errors.Join(errEnvMissing, pushErr)
	case cur.EnvMissing:
		dropped, dropErr := s.machines.DropMissing(ctx, m.ID, keep)
		if dropErr != nil {
			return "", errors.Join(errEnvMissing, dropErr)
		}
		logger.Warnf(ctx, "removed host-only entries nothing pushes again: %s", strings.Join(dropped, ", "))
	case pushErr != nil:
		logger.Warnf(ctx, "push credentials: %v", pushErr)
	}
	if wake != "" {
		return wake, nil
	}
	if err = s.machines.SetMeta(ctx, m.ID, metaWake, m.Incarnation+"|"+identity.WakeID); err != nil {
		return "", err
	}
	return identity.WakeID, nil
}

// revokeMachineKeys revokes every gw key of machine; a failure is logged, and the provisioner retries.
func (s *Service) revokeMachineKeys(ctx context.Context, machine string) {
	keys, err := s.accounts.MachineKeys(ctx, machine)
	if err != nil {
		log.WithFunc("boxes.Service.revokeMachineKeys").WithField("machine", machine).Warnf(ctx, "list the gw keys: %v", err)
		return
	}
	for _, k := range keys {
		s.revokeMachineKey(ctx, machine, k.ID)
	}
}

func (s *Service) revokeMachineKey(ctx context.Context, machine, id string) {
	if err := s.accounts.RevokeMachineKey(ctx, id); err != nil {
		log.WithFunc("boxes.Service.revokeMachineKey").WithField("machine", machine).Warnf(ctx, "revoke gw key %s: %v", id, err)
	}
}

// agentContext carries m's credential to its agent on calls the control plane makes itself.
func (s *Service) agentContext(ctx context.Context, m machines.Machine) context.Context {
	wake := wakeOf(m)
	if wake == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, credential.Header, s.provision.signer.Credential(boxOfMachine(m.ID), wake))
}

// ready reports whether the control plane may call m's agent: running, not being restored or parked, and provisioned for this incarnation.
func (s *Service) ready(m machines.Machine) bool {
	active := s.ops.activeFor(boxOfMachine(m.ID))
	return m.State == machines.StateRunning && wakeOf(m) != "" && !unrestored(m) &&
		(active == nil || (active.phase != controlv1.Phase_PHASE_RESTORING && !active.park))
}

// reachable reports whether clients may reach m: ready and holding every host-only entry.
func (s *Service) reachable(m machines.Machine) bool {
	return s.ready(m) && !m.EnvMissing
}

// wakeOf returns the wake id recorded for m's current incarnation, "" when none.
func wakeOf(m machines.Machine) string {
	incarnation, wake, ok := strings.Cut(m.Meta[metaWake], "|")
	if !ok || m.Incarnation == "" || incarnation != m.Incarnation {
		return ""
	}
	return wake
}

// endedKeys returns the running machines whose newest key is installed, and the gw keys of incarnations that no longer run and of a running one older than its newest installed key.
func endedKeys(ms []machines.Machine, keys []users.MachineKey) (map[string]bool, []users.MachineKey) {
	running := map[string]string{}
	for _, m := range ms {
		if m.State == machines.StateRunning {
			running[m.ID] = m.Incarnation
		}
	}
	keyed, pruned := map[string]bool{}, map[string]bool{}
	var ended []users.MachineKey
	for _, k := range keys {
		if inc, ok := running[k.Machine]; !ok || inc != k.Incarnation || pruned[k.Machine] {
			ended = append(ended, k)
			continue
		}
		if _, seen := keyed[k.Machine]; !seen {
			keyed[k.Machine] = k.Installed
		}
		pruned[k.Machine] = k.Installed
	}
	return keyed, ended
}
