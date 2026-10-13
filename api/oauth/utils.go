package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
)

// sharedAddressSpace is carrier-grade NAT space (RFC 6598), which IsPrivate does not cover.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// IsAccessToken reports whether an Authorization header carries one of this server's access tokens.
func IsAccessToken(header string) bool {
	token, ok := bearer(header)
	return ok && strings.HasPrefix(token, AccessPrefix)
}

func bearer(header string) (string, bool) {
	if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	return header[len(bearerPrefix):], true
}

func refusePrivate(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	ip := addrPort.Addr().Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || sharedAddressSpace.Contains(ip) {
		return errors.New("refusing " + ip.String() + ": not a public address")
	}
	return nil
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
