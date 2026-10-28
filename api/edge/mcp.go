package edge

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/credential"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/mcpwire"
	"github.com/loomhand/cocoonbox/api/oauth"
)

const (
	// MCPPrefix starts every remote-tools path: MCPPrefix + <box id> [+ subpath].
	MCPPrefix = "/cocoonbox.mcp/"

	agentMCPPath     = "/mcp"
	agentIdleTimeout = time.Minute
	jsonRPCServer    = -32000
)

// errNoBox answers a box its grant does not reach as the resolver answers another user's box.
var errNoBox = status.Error(codes.NotFound, "no such box for this user (it may have been deleted)")

// Grants checks the OAuth access tokens of agents and lists the boxes each grant created.
type Grants interface {
	Authenticate(ctx context.Context, header string) (oauth.Grant, error)
	Boxes(ctx context.Context, grant string) ([]string, error)
	Challenge(invalid bool) string
}

// route is where one MCP exchange goes and as whom: the caller for the log, the context that ends with its user and credential, and the grant of a share call.
type route struct {
	caller string
	ctx    context.Context
	stop   context.CancelFunc
	ep     machines.Endpoint
	wake   string
	grant  *auth.Grant
	left   func()
}

type boxTransport struct {
	key       string
	transport *http.Transport
}

// MCP proxies MCP Streamable HTTP exchanges to the agent of the caller's box.
type MCP struct {
	resolver Resolver
	authn    *auth.Authenticator
	creds    Credentials
	grants   Grants

	mu         sync.Mutex
	transports map[string]*boxTransport
}

// NewMCP builds the MCP proxy that authenticates to agents with box credentials from creds.
func NewMCP(resolver Resolver, authn *auth.Authenticator, creds Credentials) *MCP {
	return &MCP{resolver: resolver, authn: authn, creds: creds, transports: map[string]*boxTransport{}}
}

// EnableGrants admits an agent's OAuth access token on the boxes its grant created or the user chose for it, and on nothing else.
func (m *MCP) EnableGrants(grants Grants) {
	m.grants = grants
}

func (m *MCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, MCPPrefix) {
		http.NotFound(w, r)
		return
	}
	start := time.Now()
	boxID, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, MCPPrefix), "/")
	rt, err := m.route(r, boxID)
	defer rt.stop()
	defer func() {
		log.WithFunc("edge.MCP.ServeHTTP").Infof(r.Context(), "%s %q user=%s box=%q addr=%s code=%s in %s",
			r.Method, r.URL.Path, rt.caller, boxID, rt.ep.Key, status.Code(err), time.Since(start).Round(time.Millisecond))
	}()
	if err != nil {
		if authorization := r.Header.Get("Authorization"); m.grants != nil && status.Code(err) == codes.Unauthenticated && !sharing(authorization) {
			w.Header().Set("WWW-Authenticate", m.grants.Challenge(authorization != ""))
		}
		writeJSONRPCError(w, err)
		return
	}
	defer rt.left()
	r = r.WithContext(rt.ctx)
	if rt.grant != nil {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go cutOnEnd(ctx, cancel, rt.grant, r.Method == http.MethodGet)
		r = r.WithContext(ctx)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { m.rewrite(pr, rt, boxID) },
		Transport:     m.transportFor(boxID, rt.ep),
		FlushInterval: -1,
		ErrorHandler:  m.upstreamError,
	}
	proxy.ServeHTTP(w, r)
}

// Exchange sends req to the MCP endpoint of the agent of user's box with the owner's credential and hands use the response; the call is a reach until use returns.
// A box that is not running is Unavailable before anything is sent; a request that may have reached the agent without an answer is Unknown.
func (m *MCP) Exchange(req *http.Request, user, box string, use func(*http.Response) error) error {
	ep, wake, err := m.resolver.Resolve(req.Context(), user, box)
	if err != nil {
		return err
	}
	defer m.resolver.Reached(box)()
	req.URL.Scheme, req.URL.Host, req.URL.Path, req.Host = "http", ep.Key, agentMCPPath, ep.Key
	if wake != "" {
		req.Header.Set(credential.Header, m.creds.Credential(box, wake))
	}
	resp, err := m.transportFor(box, ep).RoundTrip(req)
	if err != nil {
		if req.Context().Err() == nil {
			log.WithFunc("edge.MCP.Exchange").Warnf(req.Context(), "send %s to the agent of box %s: %v", req.Method, box, err)
		}
		return status.Error(codes.Unknown, "box agent unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	return use(resp)
}

// route authorizes one exchange for the bearer's user, or, when no user holds it and it is a share secret, for that share.
func (m *MCP) route(r *http.Request, boxID string) (rt route, err error) {
	rt.ctx, rt.stop = r.Context(), func() {}
	authorization := r.Header.Get("Authorization")
	if m.grants != nil && oauth.IsAccessToken(authorization) {
		return m.grantRoute(r, authorization, boxID)
	}
	owner, err := m.authn.Authenticate(r.Context(), authorization)
	var user, credential string
	if secret, ok := auth.ShareSecret(authorization); err != nil && ok && boxID != "" {
		var guest string
		if guest, err = m.authn.Guest(r.Context(), r.Header.Get(auth.HeaderGuestToken)); err != nil {
			return rt, err
		}
		var grant auth.Grant
		if grant, err = m.authn.Admit(secret, boxID, guest); err != nil {
			return rt, err
		}
		rt.grant, owner, rt.caller = &grant, grant.Owner, grant.Principal()
		user, credential = guest, auth.TokenCredential(r.Header.Get(auth.HeaderGuestToken))
	} else {
		if err != nil {
			return rt, err
		}
		token, _ := auth.Bearer(authorization)
		rt.caller, user, credential = owner, owner, auth.TokenCredential(token)
	}
	if user != "" {
		rt.ctx, rt.stop = m.authn.Bind(r.Context(), user, credential)
	}
	if boxID == "" {
		return rt, status.Error(codes.InvalidArgument, "box id is required: "+MCPPrefix+"<box id>")
	}
	if rt.ep, rt.wake, err = m.resolver.Resolve(rt.ctx, owner, boxID); err == nil {
		rt.left = m.reached(r, boxID)
	}
	return rt, err
}

// reached records an exchange that keeps box up: every one but a GET, whose event stream only watches the box.
func (m *MCP) reached(r *http.Request, box string) func() {
	if r.Method == http.MethodGet {
		return func() {}
	}
	return m.resolver.Reached(box)
}

// grantRoute authorizes one exchange for an access token on a box its grant created or the user chose for it, as the box owner.
func (m *MCP) grantRoute(r *http.Request, authorization, boxID string) (rt route, err error) {
	rt.ctx, rt.stop = r.Context(), func() {}
	g, err := m.grants.Authenticate(r.Context(), authorization)
	switch {
	case errors.Is(err, oauth.ErrInvalidToken):
		return rt, status.Error(codes.Unauthenticated, "invalid access token")
	case err != nil:
		log.WithFunc("edge.MCP.grantRoute").Error(r.Context(), err, "check access token")
		return rt, status.Error(codes.Unavailable, "checking the access token failed")
	}
	rt.caller = g.User + " grant=" + g.ID
	rt.ctx, rt.stop = m.authn.Bind(r.Context(), g.User, g.ID)
	ids, err := m.grants.Boxes(rt.ctx, g.ID)
	switch {
	case err != nil:
		log.WithFunc("edge.MCP.grantRoute").Errorf(r.Context(), err, "list the boxes of grant %s", g.ID)
		return rt, status.Error(codes.Unavailable, "reading the client's boxes failed")
	case boxID == "":
		return rt, status.Error(codes.InvalidArgument, "box id is required: "+MCPPrefix+"<box id>")
	case !slices.Contains(ids, boxID) && !slices.Contains(g.Chosen, boxID):
		return rt, errNoBox
	}
	if rt.ep, rt.wake, err = m.resolver.Resolve(rt.ctx, g.User, boxID); err == nil {
		rt.left = m.reached(r, boxID)
	}
	return rt, err
}

// transportFor returns boxID's h2c transport, replacing it when the box's endpoint moved.
func (m *MCP) transportFor(boxID string, ep machines.Endpoint) *http.Transport {
	m.mu.Lock()
	defer m.mu.Unlock()
	if bt := m.transports[boxID]; bt != nil {
		if bt.key == ep.Key {
			return bt.transport
		}
		bt.transport.CloseIdleConnections()
	}
	t := newH2CTransport(func(ctx context.Context, _, _ string) (net.Conn, error) { return ep.Dial(ctx) })
	m.transports[boxID] = &boxTransport{key: ep.Key, transport: t}
	return t
}

func (m *MCP) rewrite(pr *httputil.ProxyRequest, rt route, box string) {
	_, rest, _ := strings.Cut(strings.TrimPrefix(pr.In.URL.Path, MCPPrefix), "/")
	pr.Out.URL.Scheme = "http"
	pr.Out.URL.Host = rt.ep.Key
	pr.Out.URL.Path = agentMCPPath
	if rest != "" {
		pr.Out.URL.Path += "/" + rest
	}
	pr.Out.URL.RawPath = ""
	pr.Out.Host = pr.Out.URL.Host
	pr.Out.Header.Del("Authorization")
	pr.Out.Header.Del(auth.HeaderGuestToken)
	pr.Out.Header.Del(credential.Header)
	pr.Out.Header.Del(HeaderEdgeToken)
	switch {
	case rt.wake == "":
	case rt.grant != nil:
		pr.Out.Header.Set(credential.Header, m.creds.ShareCredential(box, rt.wake, rt.grant.Principal()))
	default:
		pr.Out.Header.Set(credential.Header, m.creds.Credential(box, rt.wake))
	}
}

func (m *MCP) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	log.WithFunc("edge.MCP.upstreamError").Warnf(r.Context(), "proxy %s to box agent: %v", r.URL.Path, err)
	writeJSONRPCError(w, status.Error(codes.Unavailable, "box agent unreachable"))
}

// cutOnEnd cancels a share's exchange when the share ends, and a server stream (GET) also when write switches, so the client reconnects with the new mode.
func cutOnEnd(ctx context.Context, cancel context.CancelFunc, g *auth.Grant, stream bool) {
	var moded <-chan struct{}
	if stream {
		moded = g.Moded
	}
	select {
	case <-g.Cut:
	case <-moded:
	case <-ctx.Done():
	}
	cancel()
}

// sharing reports whether an Authorization header carries a share secret.
func sharing(authorization string) bool {
	_, ok := auth.ShareSecret(authorization)
	return ok
}

func newH2CTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	return &http.Transport{
		Protocols:       &protocols,
		DialContext:     dial,
		IdleConnTimeout: agentIdleTimeout,
	}
}

type jsonRPCError struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Error   mcpwire.Error `json:"error"`
}

func writeJSONRPCError(w http.ResponseWriter, err error) {
	body := jsonRPCError{JSONRPC: mcpwire.Version, Error: mcpwire.Error{Code: jsonRPCServer, Message: status.Convert(err).Message()}}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus(status.Code(err)))
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func httpStatus(code codes.Code) int {
	switch code {
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.NotFound:
		return http.StatusNotFound
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}
