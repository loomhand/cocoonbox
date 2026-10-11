package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/CMGS/cocoonbox/api/keyring"
)

// sealer seals key values with AES-256-GCM under a key derived from the owner's state key.
type sealer struct {
	keys keyring.Keyring
}

func (s *sealer) seal(ctx context.Context, owner, value, ad string) ([]byte, error) {
	aead, err := s.aead(ctx, owner)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(value)+aead.Overhead())
	_, _ = rand.Read(nonce)
	return aead.Seal(nonce, nonce, []byte(value), []byte(ad)), nil
}

func (s *sealer) open(ctx context.Context, owner string, sealed []byte, ad string) (string, error) {
	aead, err := s.aead(ctx, owner)
	if err != nil {
		return "", err
	}
	n := aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("sealed value is truncated")
	}
	plain, err := aead.Open(nil, sealed[:n], sealed[n:], []byte(ad))
	if err != nil {
		return "", fmt.Errorf("open %s: %w", ad, err)
	}
	return string(plain), nil
}

func (s *sealer) aead(ctx context.Context, owner string) (cipher.AEAD, error) {
	k, err := s.keys.StateKey(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("state key of %s: %w", owner, err)
	}
	key, err := hkdf.Key(sha256.New, k.Secret, nil, sealInfo, keyring.KeySize)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func valueData(id, owner string) string {
	return "vault_secrets|" + id + "|" + owner + "|value"
}
