package keyring

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// Sealer seals values with AES-256-GCM under HKDF-SHA256 of the owner's state key with Info: a nonce, then the ciphertext.
type Sealer struct {
	Keys Keyring
	Info string
}

// Seal encrypts plain for owner with ad as the associated data.
func (s Sealer) Seal(ctx context.Context, owner string, plain []byte, ad string) ([]byte, error) {
	aead, err := s.aead(ctx, owner)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	_, _ = rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, []byte(ad)), nil
}

// Open decrypts what Seal made for owner with the same ad.
func (s Sealer) Open(ctx context.Context, owner string, sealed []byte, ad string) ([]byte, error) {
	aead, err := s.aead(ctx, owner)
	if err != nil {
		return nil, err
	}
	n := aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed value is truncated")
	}
	plain, err := aead.Open(nil, sealed[:n], sealed[n:], []byte(ad))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", ad, err)
	}
	return plain, nil
}

func (s Sealer) aead(ctx context.Context, owner string) (cipher.AEAD, error) {
	k, err := s.Keys.StateKey(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("state key of %s: %w", owner, err)
	}
	key, err := hkdf.Key(sha256.New, k.Secret, nil, s.Info, KeySize)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
