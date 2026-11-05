package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestVerifies(t *testing.T) {
	verifier := strings.Repeat("a", minVerifierLen)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	short := verifier[:minVerifierLen-1]
	shortSum := sha256.Sum256([]byte(short))
	if !verifies(verifier, challenge) {
		t.Error("the verifier of the challenge is refused")
	}
	if verifies(verifier+"b", challenge) {
		t.Error("another verifier is admitted")
	}
	if verifies(short, base64.RawURLEncoding.EncodeToString(shortSum[:])) {
		t.Errorf("a verifier under %d characters is admitted", minVerifierLen)
	}
}
