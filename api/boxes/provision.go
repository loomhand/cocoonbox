package boxes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/metadata"

	"github.com/CMGS/cocoonbox/api/credential"
	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	metaWake = "wake"
	metaGW   = "gw"

	provisionAttempts = 5
	provisionBackoff  = 2 * time.Second
)

var (
	writeIdentity = []string{"sh", "-c", `set -e; umask 022; mkdir -p /run/cocoonbox-identity; chmod 0755 /run/cocoonbox-identity
t=$(mktemp /run/cocoonbox-identity/.identity.XXXXXX); cat > "$t"; chmod 0644 "$t"; mv -f "$t" /run/cocoonbox-identity/identity`}
	writeGWKey = []string{"sh", "-c", `set -e; read -r key; { grep -v '^GW_API_KEY=' /etc/cocoonbox/agent.env; printf 'GW_API_KEY=%s\n' "$key"; } |
/usr/local/lib/cocoonbox/set-agent-env; systemctl restart cocoonbox-agent`}
)

// Signer issues the credentials box agents verify with the key named in their identity file.
type Signer interface {
	KeyID() string
	PublicKey() string
	Credential(box, wake string) string
}

// GWKeys issues and revokes per-machine gw keys.
type GWKeys interface {
	Add(ctx context.Context, machine string) (string, error)
	Revoke(ctx context.Context, machine string) error
}

type identityFile struct {
	BoxID     string `json:"box_id"`
	WakeID    string `json:"wake_id"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// EnableProvisioning gives the machines of users identity files for signer and, when gw is set, per-machine gw keys.
func (s *Service) EnableProvisioning(signer Signer, gw GWKeys, users []string) {
	s.provision = &provisioner{signer: signer, gw: gw, users: users, backoff: provisionBackoff, locks: map[string]*sync.Mutex{}}
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
		if m.State == machines.StateRunning && s.provision.covers(m) && wakeOf(m) == "" {
			_ = s.provisionMachine(ctx, m)
		}
	}
}

// provisionMachine provisions m unless its current incarnation already has a wake id.
func (s *Service) provisionMachine(ctx context.Context, m machines.Machine) error {
	p := s.provision
	unlock := p.lock(m.ID)
	defer unlock()
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
	if p.gw != nil && m.Meta[metaGW] == "" {
		key, err := p.gw.Add(ctx, m.ID)
		if err != nil {
			return "", fmt.Errorf("gw key: %w", err)
		}
		if _, err := s.machines.Exec(ctx, m.ID, writeGWKey, []byte(key+"\n")); err != nil {
			return "", fmt.Errorf("install gw key: %w", err)
		}
		if err := s.machines.SetMeta(ctx, m.ID, metaGW, "1"); err != nil {
			return "", err
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("wake id: %w", err)
	}
	wake := hex.EncodeToString(raw)
	file, err := json.Marshal(identityFile{BoxID: BoxID(m.Owner), WakeID: wake, KeyID: p.signer.KeyID(), PublicKey: p.signer.PublicKey()})
	if err != nil {
		return "", fmt.Errorf("encode identity: %w", err)
	}
	if _, err := s.machines.Exec(ctx, m.ID, writeIdentity, file); err != nil {
		return "", fmt.Errorf("write identity: %w", err)
	}
	if err := s.machines.SetMeta(ctx, m.ID, metaWake, m.Incarnation+"|"+wake); err != nil {
		return "", err
	}
	return wake, nil
}

// agentContext carries m's credential to its agent on calls the control plane makes itself.
func (s *Service) agentContext(ctx context.Context, m machines.Machine) context.Context {
	wake := wakeOf(m)
	if s.provision == nil || wake == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, credential.Header, s.provision.signer.Credential(BoxID(m.Owner), wake))
}

// revokeGW drops a deleted machine's gw key.
func (s *Service) revokeGW(ctx context.Context, machine string) {
	if s.provision == nil || s.provision.gw == nil {
		return
	}
	if err := s.provision.gw.Revoke(ctx, machine); err != nil {
		log.WithFunc("boxes.Service.revokeGW").WithField("machine", machine).Error(ctx, err, "revoke gw key")
	}
}

// ready reports whether clients may reach m: running and, when m is provisioned, provisioned for this incarnation.
func (s *Service) ready(m machines.Machine) bool {
	return m.State == machines.StateRunning && (s.provision == nil || !s.provision.covers(m) || wakeOf(m) != "")
}

// provisioner writes each machine's identity file on every boot and its gw key on first boot.
type provisioner struct {
	signer  Signer
	gw      GWKeys
	users   []string
	backoff time.Duration

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (p *provisioner) covers(m machines.Machine) bool {
	return p != nil && slices.Contains(p.users, m.Owner)
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
