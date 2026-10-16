package keyring

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"testing"
)

func TestSealerKeepsTheStoredFormat(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, KeySize)
	s := Sealer{Keys: fixedKeys{secret: secret}, Info: "cocoonbox vault value"}
	nonce := bytes.Repeat([]byte{1}, 12)
	stored := append(bytes.Clone(nonce), stdGCM(t, secret, "cocoonbox vault value").Seal(nil, nonce, []byte("sk-123"), []byte("ad"))...)
	plain, err := s.Open(t.Context(), "u-1", stored, "ad")
	if err != nil || string(plain) != "sk-123" {
		t.Fatalf("Open of a stored value = %q, %v", plain, err)
	}
	sealed, err := s.Seal(t.Context(), "u-1", []byte("sk-456"), "ad")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got, err := stdGCM(t, secret, "cocoonbox vault value").Open(nil, sealed[:12], sealed[12:], []byte("ad")); err != nil || string(got) != "sk-456" {
		t.Errorf("a sealed value opens with plain AES-GCM as %q, %v", got, err)
	}
	if _, err := s.Open(t.Context(), "u-1", stored, "other"); err == nil {
		t.Error("Open accepted other associated data")
	}
	if _, err := (Sealer{Keys: s.Keys, Info: "cocoonbox flows"}).Open(t.Context(), "u-1", stored, "ad"); err == nil {
		t.Error("Open accepted a value sealed under another info")
	}
	if _, err := s.Open(t.Context(), "u-1", stored[:5], "ad"); err == nil {
		t.Error("Open accepted a truncated value")
	}
}

type fixedKeys struct {
	secret []byte
}

func (f fixedKeys) StateKey(context.Context, string) (Key, error) {
	return Key{ID: "k", Secret: f.secret}, nil
}

func stdGCM(t *testing.T, secret []byte, info string) cipher.AEAD {
	t.Helper()
	key, err := hkdf.Key(sha256.New, secret, nil, info, KeySize)
	if err != nil {
		t.Fatalf("hkdf: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	return gcm
}
