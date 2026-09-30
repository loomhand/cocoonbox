// Package keyring defines where per-owner state keys live.
package keyring

import "context"

// KeySize is the length of a state key in bytes (AES-256, HMAC-SHA256).
const KeySize = 32

// Key encrypts one owner's state.
type Key struct {
	ID     string
	Secret []byte
}

// Keyring returns an owner's key, creating it on first use.
type Keyring interface {
	Key(ctx context.Context, owner string) (Key, error)
}
