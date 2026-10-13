package oauth

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
)

const (
	maxForm        = 64 << 10
	minVerifierLen = 43
	maxVerifierLen = 128
	unnamedClient  = "Unnamed client"
)

// ServeToken redeems a code or a refresh token for an access token and a new refresh token.
func (s *Service) ServeToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body must be a form")
		return
	}
	f := r.PostForm
	if resource := f.Get("resource"); resource != "" && !s.covers(resource) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource must be "+s.resource)
		return
	}
	clientID := cmp.Or(f.Get("client_id"), basicUser(r))
	access, refresh := randomToken(AccessPrefix), randomToken(refreshPrefix)
	tokens := issued{access: hashHex(access), refresh: hashHex(refresh)}
	var err error
	switch f.Get("grant_type") {
	case authorizationCode:
		err = s.store.redeem(r.Context(), hashHex(f.Get(codeParam)), grantPrefix+randomHex(grantIDBytes), tokens, func(c code) error {
			if time.Now().After(c.expiresAt) || c.client.id != clientID || c.redirectURI != f.Get("redirect_uri") || !verifies(f.Get("code_verifier"), c.challenge) {
				return errInvalidGrant
			}
			return nil
		})
	case refreshToken:
		err = s.store.refresh(r.Context(), hashHex(f.Get(refreshToken)), clientID, tokens)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
		return
	}
	switch {
	case errors.Is(err, errInvalidGrant):
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the code or refresh token is invalid, expired, spent, or for another client")
		return
	case err != nil:
		log.WithFunc("oauth.Service.ServeToken").Error(r.Context(), err, "issue tokens")
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "issuing tokens failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(accessTTL.Seconds()),
		refreshToken:   refresh,
		"scope":        Scope,
	})
}

// ServeRegister registers a public client without storing it: the client id carries its name and redirect URIs, signed.
func (s *Service) ServeRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxForm), &req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be a JSON client metadata object")
		return
	}
	name := cmp.Or(strings.TrimSpace(req.ClientName), unnamedClient)
	if err := checkClient(name, req.RedirectURIs); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
		return
	}
	id, err := s.register(name, req.RedirectURIs)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "registration failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                grantTypes,
		"response_types":             []string{codeParam},
		"token_endpoint_auth_method": authNone,
		"scope":                      Scope,
	})
}

// ServeRevoke ends the grant of an access or refresh token its client sends; an unknown token is no error (RFC 7009).
func (s *Service) ServeRevoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body must be a form")
		return
	}
	token := r.PostForm.Get("token")
	if strings.HasPrefix(token, AccessPrefix) || strings.HasPrefix(token, refreshPrefix) {
		if err := s.store.revokeByToken(r.Context(), hashHex(token), cmp.Or(r.PostForm.Get("client_id"), basicUser(r))); err != nil {
			log.WithFunc("oauth.Service.ServeRevoke").Error(r.Context(), err, "revoke grant")
			writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "revoking failed")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// verifies checks a PKCE S256 verifier against its challenge.
func verifies(verifier, challenge string) bool {
	if len(verifier) < minVerifierLen || len(verifier) > maxVerifierLen {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}
