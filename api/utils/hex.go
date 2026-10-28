// Package utils holds the helpers that several api packages share.
package utils

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

func RandomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func HashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
