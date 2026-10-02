package credential

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCredentialVerifiesAndIsCached(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	s := New(priv)
	clock := time.Unix(1_790_000_000, 0)
	s.now = func() time.Time { return clock }

	c := s.Credential("cb-ef260e9aa3c6", "w1")
	parts := strings.Split(c, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		t.Fatalf("credential %q has no v1.<fields>.<sig> shape", c)
	}
	sig, err := encoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatalf("signature does not verify: %v", err)
	}
	fields, _ := encoding.DecodeString(parts[1])
	want := strings.Join([]string{s.KeyID(), "cb-ef260e9aa3c6", "w1", strconv.FormatInt(clock.Add(lifetime).Unix(), 10)}, "|")
	if string(fields) != want {
		t.Errorf("fields = %q, want %q", fields, want)
	}
	if pk, _ := encoding.DecodeString(s.PublicKey()); !pub.Equal(ed25519.PublicKey(pk)) || len(s.KeyID()) != 16 {
		t.Errorf("public key %q / key id %q", s.PublicKey(), s.KeyID())
	}

	clock = clock.Add(7 * time.Minute)
	if again := s.Credential("cb-ef260e9aa3c6", "w1"); again != c {
		t.Error("credential re-signed with 3 min left")
	}
	if other := s.Credential("cb-ef260e9aa3c6", "w2"); other == c {
		t.Error("new wake reused the old credential")
	}
	clock = clock.Add(90 * time.Second)
	if fresh := s.Credential("cb-ef260e9aa3c6", "w1"); fresh == c {
		t.Error("credential not refreshed with 90 s left")
	}
}

func TestShareCredentialNamesThePrincipal(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	s := New(priv)
	owner := s.Credential("cb-ef260e9aa3c6", "w1")
	read := s.ShareCredential("cb-ef260e9aa3c6", "w1", "share:sh-1:read")
	parts := strings.Split(read, ".")
	if len(parts) != 3 || parts[0] != "v2" {
		t.Fatalf("credential %q has no v2.<fields>.<sig> shape", read)
	}
	sig, _ := encoding.DecodeString(parts[2])
	fields, _ := encoding.DecodeString(parts[1])
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) || !strings.HasSuffix(string(fields), "|share:sh-1:read") || strings.Count(string(fields), "|") != 4 {
		t.Errorf("fields %q do not verify or end with the principal", fields)
	}
	if write := s.ShareCredential("cb-ef260e9aa3c6", "w1", "share:sh-1:write"); write == read || owner == read {
		t.Error("credentials of different principals are shared")
	}
	if again := s.Credential("cb-ef260e9aa3c6", "w1"); again != owner || !strings.HasPrefix(owner, "v1.") {
		t.Error("the owner's credential changed after share credentials were issued")
	}
}

func TestLoadReadsPKCS8(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "edge-signing.key")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := Load(path)
	if err != nil || s.KeyID() != New(priv).KeyID() {
		t.Errorf("Load = %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("Load of a missing file succeeded")
	}
}

func BenchmarkCredentialCached(b *testing.B) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := New(priv)
	s.Credential("cb-ef260e9aa3c6", "w1")
	for b.Loop() {
		s.Credential("cb-ef260e9aa3c6", "w1")
	}
}

func BenchmarkCredentialSign(b *testing.B) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := New(priv)
	clock := time.Unix(1_790_000_000, 0)
	s.now = func() time.Time { clock = clock.Add(lifetime); return clock }
	for b.Loop() {
		s.Credential("cb-ef260e9aa3c6", "w1")
	}
}
