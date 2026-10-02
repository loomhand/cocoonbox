// Package keyring defines the per-user state key.
package keyring

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"
)

// KeySize is the length of a state key in bytes (AES-256, HMAC-SHA256).
const KeySize = 32

// Key encrypts one user's state.
type Key struct {
	ID     string
	Secret []byte
}

// Keyring returns a user's state key.
type Keyring interface {
	StateKey(ctx context.Context, user string) (Key, error)
}

// Generate draws a new key for user.
func Generate(user string) Key {
	k := Key{ID: fmt.Sprintf("users:%s:%d", user, time.Now().Unix()), Secret: make([]byte, KeySize)}
	_, _ = rand.Read(k.Secret)
	return k
}
