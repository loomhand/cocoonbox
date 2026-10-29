package oauth

import (
	"encoding/json/v2"
	"net/http"
	"strings"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/utils"
)

// IsAccessToken reports whether an Authorization header carries one of this server's access tokens.
func IsAccessToken(header string) bool {
	token, ok := auth.Bearer(header)
	return ok && strings.HasPrefix(token, AccessPrefix)
}

func randomToken(prefix string) string {
	return prefix + utils.RandomHex(secretBytes)
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
	_ = json.MarshalWrite(w, v, json.Deterministic(true))
}
