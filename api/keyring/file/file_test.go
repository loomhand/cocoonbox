package file

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/CMGS/cocoonbox/api/keyring"
)

func TestKeyIsCreatedOnceAndPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	k, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, err := k.Key(t.Context(), "cb-2bd806c97f0e")
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if len(first.Secret) != keyring.KeySize || first.ID == "" {
		t.Fatalf("key = %d bytes, id %q", len(first.Secret), first.ID)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatalf("New again: %v", err)
	}
	again, err := reopened.Key(t.Context(), "cb-2bd806c97f0e")
	if err != nil || again.ID != first.ID || !bytes.Equal(again.Secret, first.Secret) {
		t.Errorf("reopened key differs: %v", err)
	}
	other, err := k.Key(t.Context(), "cb-000000000000")
	if err != nil || bytes.Equal(other.Secret, first.Secret) {
		t.Errorf("other owner got the same key: %v", err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "cb-2bd806c97f0e.json")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if want := map[bool]os.FileMode{true: 0o700, false: 0o600}[info.IsDir()]; info.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", path, info.Mode().Perm(), want)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("keyring dir holds %d entries, want 2 key files", len(entries))
	}
}

func TestKeyRejectsBadOwners(t *testing.T) {
	k, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, owner := range []string{"", "../etc", "CB-UPPER", "a/b"} {
		if _, err := k.Key(t.Context(), owner); err == nil {
			t.Errorf("Key(%q) accepted", owner)
		}
	}
}
