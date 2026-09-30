// Package file keeps state keys as 0600 files in one directory.
package file

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/CMGS/cocoonbox/api/keyring"
)

var ownerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type record struct {
	ID     string `json:"id"`
	Secret []byte `json:"secret"`
}

var _ keyring.Keyring = (*Keyring)(nil)

// Keyring stores one JSON file per owner under a directory.
type Keyring struct {
	dir string
	mu  sync.Mutex
}

// New uses dir, creating it with mode 0700.
func New(dir string) (*Keyring, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create keyring dir: %w", err)
	}
	return &Keyring{dir: dir}, nil
}

func (k *Keyring) Key(_ context.Context, owner string) (keyring.Key, error) {
	if !ownerPattern.MatchString(owner) {
		return keyring.Key{}, fmt.Errorf("invalid key owner %q", owner)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	path := filepath.Join(k.dir, owner+".json")
	rec, err := read(path)
	if errors.Is(err, fs.ErrNotExist) {
		rec, err = create(path, owner)
	}
	if err != nil {
		return keyring.Key{}, err
	}
	return keyring.Key{ID: rec.ID, Secret: rec.Secret}, nil
}

func read(path string) (record, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is built from a validated owner
	if err != nil {
		return record{}, err
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return record{}, fmt.Errorf("decode key %s: %w", path, err)
	}
	if len(rec.Secret) != keyring.KeySize {
		return record{}, fmt.Errorf("key %s has %d bytes, want %d", path, len(rec.Secret), keyring.KeySize)
	}
	return rec, nil
}

func create(path, owner string) (record, error) {
	rec := record{ID: fmt.Sprintf("file:%s:%d", owner, time.Now().Unix()), Secret: make([]byte, keyring.KeySize)}
	if _, err := rand.Read(rec.Secret); err != nil {
		return record{}, fmt.Errorf("generate key: %w", err)
	}
	raw, err := json.Marshal(rec) //nolint:gosec // the keyring file exists to hold this secret
	if err != nil {
		return record{}, fmt.Errorf("encode key: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+owner+".*")
	if err != nil {
		return record{}, fmt.Errorf("create key: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return record{}, fmt.Errorf("write key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return record{}, fmt.Errorf("sync key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return record{}, fmt.Errorf("close key: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		return record{}, fmt.Errorf("install key %s: %w", path, err)
	}
	return rec, nil
}
