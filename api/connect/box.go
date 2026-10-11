package connect

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	// guestEnv is the guest entry whose records the image renders into box shells, as it does the vault's.
	guestEnv = "COCOONBOX_CONNECT"
	flagOn   = "1"
)

// DoorKey issues machine's box door key for its incarnation; the keys of its earlier incarnations stop working.
func (s *Service) DoorKey(ctx context.Context, machine, owner, incarnation string) (string, error) {
	key := randomToken()
	if err := s.store.putDoorKey(ctx, hashHex(key), machine, owner, incarnation); err != nil {
		return "", err
	}
	return key, nil
}

// DropDoorKey deletes the door key of machine, which is gone.
func (s *Service) DropDoorKey(ctx context.Context, machine string) error {
	return s.store.dropDoorKeys(ctx, machine)
}

// PruneDoorKeys deletes the door keys issued before cutoff to machines not in live and returns how many went.
func (s *Service) PruneDoorKeys(ctx context.Context, live []string, cutoff time.Time) (int64, error) {
	return s.store.pruneDoorKeys(ctx, live, cutoff)
}

// Credentials returns what owner's grants put on the egress of owner's boxes, refreshing a token near its expiry.
// A connector without an active grant maps to removals; one whose token cannot be read now is left out, with guestEnv.
func (s *Service) Credentials(ctx context.Context, owner string) (machines.Credentials, error) {
	grants, err := s.store.grants(ctx, owner)
	if err != nil {
		return machines.Credentials{}, err
	}
	out := machines.Credentials{Secrets: map[string]*machines.Secret{}, Flags: map[string]string{}}
	var records []string
	unread := false
	for _, e := range s.catalog {
		if len(e.inject) == 0 {
			continue
		}
		token, err := s.injectToken(ctx, e, grants)
		if err != nil {
			log.WithFunc("connect.Service.Credentials").Warnf(ctx, "read the %s token of %s: %v", e.id, owner, err)
			unread = unread || len(e.guest) > 0
			continue
		}
		for _, in := range e.inject {
			out.Secrets[in.name] = nil
			if token != "" {
				out.Secrets[in.name] = &machines.Secret{Value: in.value(token), Hosts: in.hosts, Header: in.header, Placeholder: in.placeholder}
			}
		}
		if e.flag != "" {
			out.Flags[e.flag] = ""
			if token != "" {
				out.Flags[e.flag] = flagOn
			}
		}
		if token != "" {
			records = append(records, e.guest...)
		}
	}
	if !unread {
		out.Flags[guestEnv] = strings.Join(records, " ")
	}
	return out, nil
}

// BoxCaller maps a box door key to its machine's owner and the machine.
func (s *Service) BoxCaller(ctx context.Context, header string) (string, string, error) {
	key, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || key == "" {
		return "", "", errNoDoorKey
	}
	return s.store.doorKey(ctx, hashHex(key))
}

// injectToken returns the access token of e's active grant, "" when there is none or the vendor no longer accepts it.
func (s *Service) injectToken(ctx context.Context, e *entry, grants []*grant) (string, error) {
	i := slices.IndexFunc(grants, func(g *grant) bool { return g.kind == e.id && g.status == statusActive })
	if i < 0 {
		return "", nil
	}
	g := grants[i]
	sec, err := s.secrets(ctx, g)
	if err != nil {
		return "", err
	}
	token, err := s.access(ctx, g, sec.AccessToken, false)
	if errors.Is(err, errReconnect) {
		return "", nil
	}
	return token, err
}
