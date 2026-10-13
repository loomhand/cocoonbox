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
	switch {
	case !verifies(verifier, challenge):
		t.Error("the verifier of the challenge is refused")
	case verifies(verifier+"b", challenge):
		t.Error("another verifier is admitted")
	case verifies(short, base64.RawURLEncoding.EncodeToString(shortSum[:])):
		t.Error("a verifier under 43 characters is admitted")
	}
}
