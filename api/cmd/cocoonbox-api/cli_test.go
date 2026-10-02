package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/CMGS/cocoonbox/api/keyring"
)

func TestImportStateKeyTakesTheBoxKeyOrDrawsOne(t *testing.T) {
	dir := t.TempDir()
	boxFile := filepath.Join(dir, "cb-9f86d081884c.json")
	secret := bytes.Repeat([]byte{9}, keyring.KeySize)
	if err := os.WriteFile(boxFile, []byte(`{"id":"file:cb-9f86d081884c:1","secret":"CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQk="}`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	keys := &fakeStateKeys{held: map[string]keyring.Key{}}
	if err := importStateKey(t.Context(), keys, "test", boxFile); err != nil {
		t.Fatalf("import box key: %v", err)
	}
	if got := keys.held["test"]; got.ID != "file:cb-9f86d081884c:1" || !bytes.Equal(got.Secret, secret) {
		t.Errorf("imported key %q", got.ID)
	}
	missing := filepath.Join(dir, "cb-ef260e9aa3c6.json")
	for range 2 {
		if err := importStateKey(t.Context(), keys, "dev", missing); err != nil {
			t.Fatalf("import without a file: %v", err)
		}
	}
	if keys.sets != 2 || len(keys.held["dev"].Secret) != keyring.KeySize {
		t.Errorf("%d keys set, dev holds %d bytes; want one new key for dev", keys.sets, len(keys.held["dev"].Secret))
	}
}

type fakeStateKeys struct {
	held map[string]keyring.Key
	sets int
}

func (f *fakeStateKeys) StateKey(_ context.Context, user string) (keyring.Key, error) {
	k, ok := f.held[user]
	if !ok {
		return keyring.Key{}, errors.New("no state key")
	}
	return k, nil
}

func (f *fakeStateKeys) SetStateKey(_ context.Context, user string, k keyring.Key) error {
	f.sets++
	f.held[user] = k
	return nil
}
