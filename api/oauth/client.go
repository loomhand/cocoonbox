package oauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	dcrPrefix      = "dcr_"
	macBytes       = 16
	maxNameLen     = 100
	maxRedirects   = 10
	maxRedirectLen = 512
	maxDocument    = 64 << 10
	maxCached      = 1000
	documentTTL    = 5 * time.Minute
	fetchTimeout   = 5 * time.Second
)

var (
	errUnknownClient = errors.New("client_id is neither a client metadata document URL nor a client this server registered")

	loopbackHosts = []string{"localhost", "127.0.0.1", "::1"}
)

// client is a client as the server trusts it: its id, the name it gives, the verified host of its metadata document
// (empty for a registered client) and the redirect URIs it may use.
type client struct {
	id        string
	name      string
	host      string
	redirects []string
}

// allows reports whether uri is one of the client's redirect URIs; a loopback one matches on any port (RFC 8252 §7.3).
func (c client) allows(uri string) bool {
	return slices.ContainsFunc(c.redirects, func(registered string) bool { return sameRedirect(registered, uri) })
}

// registration is what a dynamically registered client id carries, signed by the server, so registration stores nothing.
type registration struct {
	Name      string   `json:"n"`
	Redirects []string `json:"r"`
}

// metadataDocument is the part of a client ID metadata document the server reads.
type metadataDocument struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
	AuthMethod   string   `json:"token_endpoint_auth_method"`
}

type cachedDocument struct {
	client    client
	fetchedAt time.Time
}

// documents caches fetched metadata documents by URL for documentTTL.
type documents struct {
	mu      sync.Mutex
	entries map[string]cachedDocument
}

func (d *documents) get(id string) (client, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[id]
	if !ok || time.Since(e.fetchedAt) > documentTTL {
		return client{}, false
	}
	return e.client, true
}

func (d *documents) put(c client) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.entries) >= maxCached {
		clear(d.entries)
	}
	d.entries[c.id] = cachedDocument{client: c, fetchedAt: time.Now()}
}

// client resolves a client_id: an https URL is a metadata document to fetch, dcr_ a client this server registered.
func (s *Service) client(ctx context.Context, id string) (client, error) {
	switch {
	case strings.HasPrefix(id, "https://"):
		return s.document(ctx, id)
	case strings.HasPrefix(id, dcrPrefix):
		return s.registered(id)
	}
	return client{}, errUnknownClient
}

func (s *Service) document(ctx context.Context, id string) (client, error) {
	if c, ok := s.documents.get(id); ok {
		return c, nil
	}
	u, err := url.Parse(id)
	if err != nil || u.Host == "" || u.Path == "" || u.Path == "/" || u.Fragment != "" || u.User != nil {
		return client{}, errors.New("a client_id URL is https with a path and no fragment or user info")
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return client{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.fetch.Do(req)
	if err != nil {
		return client{}, fmt.Errorf("fetch the client metadata document: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return client{}, fmt.Errorf("fetch the client metadata document: HTTP %d", resp.StatusCode)
	}
	if ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); ct != "application/json" {
		return client{}, fmt.Errorf("the client metadata document is %q, not application/json", ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	if err != nil {
		return client{}, fmt.Errorf("read the client metadata document: %w", err)
	}
	if len(body) > maxDocument {
		return client{}, fmt.Errorf("the client metadata document exceeds %d bytes", maxDocument)
	}
	var doc metadataDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return client{}, fmt.Errorf("parse the client metadata document: %w", err)
	}
	switch {
	case doc.ClientID != id:
		return client{}, errors.New("the client metadata document names another client_id")
	case doc.AuthMethod != "" && doc.AuthMethod != authNone:
		return client{}, fmt.Errorf("token_endpoint_auth_method %q is not supported; this server takes public clients only", doc.AuthMethod)
	}
	if err := checkClient(doc.ClientName, doc.RedirectURIs); err != nil {
		return client{}, err
	}
	c := client{id: id, name: doc.ClientName, host: u.Hostname(), redirects: doc.RedirectURIs}
	s.documents.put(c)
	return c, nil
}

// register signs a client id that carries the client's name and redirect URIs.
func (s *Service) register(name string, redirects []string) (string, error) {
	payload, err := json.Marshal(registration{Name: name, Redirects: redirects})
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return dcrPrefix + encoded + "." + base64.RawURLEncoding.EncodeToString(s.mac(encoded)), nil
}

func (s *Service) registered(id string) (client, error) {
	encoded, sig, ok := strings.Cut(strings.TrimPrefix(id, dcrPrefix), ".")
	mac, err := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err != nil || !hmac.Equal(mac, s.mac(encoded)) {
		return client{}, errUnknownClient
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return client{}, errUnknownClient
	}
	var reg registration
	if err := json.Unmarshal(payload, &reg); err != nil {
		return client{}, errUnknownClient
	}
	return client{id: id, name: reg.Name, redirects: reg.Redirects}, nil
}

func (s *Service) mac(encoded string) []byte {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(encoded))
	return h.Sum(nil)[:macBytes]
}

func sameRedirect(registered, uri string) bool {
	if registered == uri {
		return true
	}
	a, errA := url.Parse(registered)
	b, errB := url.Parse(uri)
	return errA == nil && errB == nil && a.Scheme == schemeHTTP && b.Scheme == schemeHTTP && isLoopback(a.Hostname()) &&
		a.Hostname() == b.Hostname() && a.EscapedPath() == b.EscapedPath() && a.RawQuery == b.RawQuery && b.Fragment == "" && b.User == nil
}

// checkClient refuses a client without a name or with a redirect URI the browser must not be sent to.
func checkClient(name string, redirects []string) error {
	switch {
	case strings.TrimSpace(name) == "" || len(name) > maxNameLen:
		return fmt.Errorf("client_name must be 1 to %d bytes", maxNameLen)
	case len(redirects) == 0 || len(redirects) > maxRedirects:
		return fmt.Errorf("redirect_uris must list 1 to %d URIs", maxRedirects)
	}
	for _, uri := range redirects {
		if err := checkRedirect(uri); err != nil {
			return err
		}
	}
	return nil
}

// checkRedirect admits https, http on a loopback host, and a private-use scheme of a native app (RFC 8252 §7.1).
func checkRedirect(uri string) error {
	u, err := url.Parse(uri)
	switch {
	case err != nil || len(uri) > maxRedirectLen || u.Fragment != "" || u.User != nil:
		return fmt.Errorf("redirect URI %q is not a URI without a fragment", uri)
	case u.Scheme == "https" && u.Host != "",
		u.Scheme == schemeHTTP && isLoopback(u.Hostname()),
		strings.Contains(u.Scheme, "."):
		return nil
	}
	return fmt.Errorf("redirect URI %q must be https, http on localhost, or an app's own scheme", uri)
}

// redirectHost names where a redirect URI sends the browser, as the consent page shows it.
func redirectHost(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil {
		return uri, false
	}
	switch {
	case u.Scheme == schemeHTTP && isLoopback(u.Hostname()):
		return u.Host, true
	case u.Scheme == "https":
		return u.Host, false
	}
	return u.Scheme + ":", false
}

func isLoopback(host string) bool {
	return slices.Contains(loopbackHosts, host)
}
