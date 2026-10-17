package users

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/projecteru2/core/log"
)

const (
	devicePrefix    = "dev_"
	deviceIDBytes   = 8
	deviceUsedEvery = time.Minute
)

// Device is a client's sign-in on one device, as the console lists it; it never carries the token.
type Device struct {
	ID        string     `json:"id"`
	Client    string     `json:"client"`
	Label     string     `json:"label"`
	CreatedAt time.Time  `json:"created_at"`
	UsedAt    *time.Time `json:"used_at"`
}

type deviceToken struct {
	id     string
	sha256 string
}

// IssueDevice gives the enabled user id a device token for client, labeled label, and returns it once.
func (s *Service) IssueDevice(ctx context.Context, id, client, label string) (string, error) {
	r, err := s.record(id)
	if err != nil {
		return "", err
	}
	if r.Disabled {
		return "", ErrNotFound
	}
	token := TokenPrefix + randomHex(secretBytes)
	d := deviceToken{id: devicePrefix + randomHex(deviceIDBytes), sha256: hashHex(token)}
	if err := s.store.insertDevice(ctx, id, d, client, label); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.byID[id]; ok {
		cur.devices = append(slices.Clone(cur.devices), d)
		s.byID[id] = cur
		addTokens(s.byToken, cur)
	}
	return token, nil
}

// Devices returns user id's devices, newest first.
func (s *Service) Devices(ctx context.Context, id string) ([]Device, error) {
	return s.store.devices(ctx, id)
}

// TokenDevice returns the id of the device whose token hashes to sum, empty for the personal token or an unknown one.
func (s *Service) TokenDevice(sum string) string {
	owner, _ := s.indexedToken(sum)
	return owner.device
}

// RevokeDevice ends user id's device token device at once.
func (s *Service) RevokeDevice(ctx context.Context, id, device string) error {
	sum, err := s.store.removeDevice(ctx, id, device)
	if err != nil {
		return err
	}
	s.dropDevice(ctx, id, device, sum)
	return nil
}

// RevokeDeviceSHA256 ends the device token that hashes to sum; an unknown sum is no error.
func (s *Service) RevokeDeviceSHA256(ctx context.Context, sum string) error {
	id, device, err := s.store.removeDeviceBySHA256(ctx, sum)
	if err != nil || device == "" {
		return err
	}
	s.dropDevice(ctx, id, device, sum)
	return nil
}

// dropDevice takes a revoked device token out of the index and tells the other apis to reload theirs.
func (s *Service) dropDevice(ctx context.Context, id, device, sum string) {
	s.mu.Lock()
	delete(s.byToken, sum)
	if r, ok := s.byID[id]; ok {
		r.devices = slices.DeleteFunc(slices.Clone(r.devices), func(d deviceToken) bool { return d.id == device })
		s.byID[id] = r
	}
	s.endCredential(sum)
	s.mu.Unlock()
	s.deviceUse.Delete(device)
	if err := s.store.notify(ctx); err != nil {
		log.WithFunc("users.Service.dropDevice").Warnf(ctx, "revoked device %s; other apis reload on their next notify: %v", device, err)
	}
}

func (s *Service) touchDevice(ctx context.Context, device string) {
	value, ok := s.deviceUse.Load(device)
	if !ok {
		value, _ = s.deviceUse.LoadOrStore(device, new(atomic.Int64))
	}
	last, _ := value.(*atomic.Int64)
	prev, now := last.Load(), time.Now().UnixNano()
	if now-prev < int64(deviceUsedEvery) || !last.CompareAndSwap(prev, now) {
		return
	}
	if err := s.store.touchDevice(ctx, device); err != nil {
		log.WithFunc("users.Service.touchDevice").Warnf(ctx, "record the use of device %s: %v", device, err)
	}
}
