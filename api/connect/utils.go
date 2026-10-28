package connect

import (
	"net"
	"net/http"
	"time"

	"github.com/loomhand/cocoonbox/api/publicnet"
)

const (
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 10 * time.Second
	idleTimeout      = 90 * time.Second
)

// newHTTPClient dials only public addresses unless allowPrivate is set, so a custom URL cannot reach the host.
func newHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: dialTimeout}
	if !allowPrivate {
		dialer.Control = publicnet.RefusePrivate
	}
	return &http.Client{Transport: &http.Transport{
		DialContext:         dialer.DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: handshakeTimeout,
		IdleConnTimeout:     idleTimeout,
	}}
}
