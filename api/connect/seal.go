package connect

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/CMGS/cocoonbox/api/keyring"
)

type secrets struct {
	AccessToken  string `json:"access_token,omitzero"`
	RefreshToken string `json:"refresh_token,omitzero"`
	APIKey       string `json:"api_key,omitzero"`
	Header       string `json:"header,omitzero"`
	// Client is the registered client of an MCP-authorized grant; a catalog grant uses the catalog's.
	Client client `json:"client,omitzero"`
}

type flowSecrets struct {
	Verifier string   `json:"verifier"`
	Client   client   `json:"client,omitzero"`
	Scopes   []string `json:"scopes,omitzero"`
}

// sealer seals grant and flow secrets with AES-256-GCM under a key derived from the owner's state key.
type sealer struct {
	keys keyring.Keyring
}

func (s *sealer) seal(ctx context.Context, user string, v any, ad string) ([]byte, error) {
	aead, err := s.aead(ctx, user)
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	_, _ = rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, []byte(ad)), nil
}

func (s *sealer) open(ctx context.Context, user string, sealed []byte, ad string, v any) error {
	aead, err := s.aead(ctx, user)
	if err != nil {
		return err
	}
	n := aead.NonceSize()
	if len(sealed) < n {
		return errors.New("sealed value is truncated")
	}
	plain, err := aead.Open(nil, sealed[:n], sealed[n:], []byte(ad))
	if err != nil {
		return fmt.Errorf("open %s: %w", ad, err)
	}
	return json.Unmarshal(plain, v)
}

func (s *sealer) aead(ctx context.Context, user string) (cipher.AEAD, error) {
	k, err := s.keys.StateKey(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("state key of %s: %w", user, err)
	}
	key, err := hkdf.Key(sha256.New, k.Secret, nil, grantKeyInfo, keyring.KeySize)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func grantData(id, user, connector string) string {
	return "connect_grants|" + id + "|" + user + "|" + connector
}

func flowData(id, user, connector string) string {
	return "connect_flows|" + id + "|" + user + "|" + connector
}
