package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"strings"

	"github.com/loomhand/cocoonbox/api/auth"
)

// IsAccessToken reports whether an Authorization header carries one of this server's access tokens.
func IsAccessToken(header string) bool {
	token, ok := auth.Bearer(header)
	return ok && strings.HasPrefix(token, AccessPrefix)
}

func randomToken(prefix string) string {
	return prefix + randomHex(secretBytes)
}

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func hashHex(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func basicUser(r *http.Request) string {
	user, _, _ := r.BasicAuth()
	return user
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{errorParam: code, errorDescriptionParam: description})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, v)
}
