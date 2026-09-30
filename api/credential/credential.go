// Package credential signs the box-bound credentials the edge presents to box agents.
package credential

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Header carries the credential on agent gRPC calls and MCP requests.
	Header = "x-cocoonbox-credential"

	version       = "v1"
	lifetime      = 10 * time.Minute
	refreshBefore = 2 * time.Minute
)

var encoding = base64.RawURLEncoding

type issued struct {
	value string
	exp   time.Time
}

// Signer issues v1.<b64url(key_id|box_id|wake_id|exp)>.<b64url(ed25519 over "v1.<fields>")>, cached per box and wake.
type Signer struct {
	key   ed25519.PrivateKey
	keyID string
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]issued
}

// Load reads a PEM PKCS#8 ed25519 private key.
func Load(path string) (*Signer, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied credential path
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("signing key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing key: %w", err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signing key is %T, want ed25519", parsed)
	}
	return New(key), nil
}

// New signs with key.
func New(key ed25519.PrivateKey) *Signer {
	pub, _ := key.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	return &Signer{key: key, keyID: hex.EncodeToString(sum[:])[:16], now: time.Now, cache: map[string]issued{}}
}

// KeyID names the signing key: the first 16 hex digits of sha256 of the public key.
func (s *Signer) KeyID() string { return s.keyID }

// PublicKey is the raw ed25519 public key in unpadded base64url.
func (s *Signer) PublicKey() string {
	pub, _ := s.key.Public().(ed25519.PublicKey)
	return encoding.EncodeToString(pub)
}

// Credential returns a credential for box at wake, reusing one with more than refreshBefore left.
func (s *Signer) Credential(box, wake string) string {
	now := s.now()
	cacheKey := box + "|" + wake
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[cacheKey]; ok && c.exp.Sub(now) > refreshBefore {
		return c.value
	}
	for k, c := range s.cache {
		if !c.exp.After(now) {
			delete(s.cache, k)
		}
	}
	exp := now.Add(lifetime).Truncate(time.Second)
	fields := strings.Join([]string{s.keyID, box, wake, strconv.FormatInt(exp.Unix(), 10)}, "|")
	signed := version + "." + encoding.EncodeToString([]byte(fields))
	value := signed + "." + encoding.EncodeToString(ed25519.Sign(s.key, []byte(signed)))
	s.cache[cacheKey] = issued{value: value, exp: exp}
	return value
}
