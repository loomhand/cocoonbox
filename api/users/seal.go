package users

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
)

const masterKeySize = 32

// sealer encrypts each user's backing keys and each share's secret with AES-256-GCM under the master key; the additional data binds a value to its row and column.
type sealer struct {
	aead cipher.AEAD
}

func newSealer(path string) (*sealer, error) {
	key, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	if len(key) != masterKeySize {
		return nil, fmt.Errorf("master key %s must hold exactly %d bytes, got %d", path, masterKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead}, nil
}

func (s *sealer) seal(plain string, ad []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	_, _ = rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, []byte(plain), ad)
}

func (s *sealer) open(sealed, ad []byte) (string, error) {
	if len(sealed) < s.aead.NonceSize() {
		return "", errors.New("sealed value is truncated")
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], ad)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", ad, err)
	}
	return string(plain), nil
}

func userData(id, column string) []byte {
	return []byte("users|" + id + "|" + column)
}

func shareData(id, box string) []byte {
	return []byte("shares|" + id + "|" + box + "|secret")
}
