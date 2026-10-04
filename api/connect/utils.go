package connect

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

const (
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 10 * time.Second
	idleTimeout      = 90 * time.Second
)

// sharedAddressSpace is carrier-grade NAT space (RFC 6598), which IsPrivate does not cover.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// newHTTPClient dials only public addresses unless allowPrivate is set, so a custom URL cannot reach the host.
func newHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: dialTimeout}
	if !allowPrivate {
		dialer.Control = refusePrivate
	}
	return &http.Client{Transport: &http.Transport{
		DialContext:         dialer.DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: handshakeTimeout,
		IdleConnTimeout:     idleTimeout,
	}}
}

func refusePrivate(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	ip := addrPort.Addr().Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || sharedAddressSpace.Contains(ip) {
		return fmt.Errorf("refusing %s: not a public address", ip)
	}
	return nil
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
