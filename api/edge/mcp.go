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

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/credential"
	"github.com/CMGS/cocoonbox/api/machines"
	"github.com/CMGS/cocoonbox/api/oauth"
)

const (
	// MCPPrefix starts every remote-tools path: MCPPrefix + <box id> [+ subpath].
	MCPPrefix = "/cocoonbox.mcp/"

	agentMCPPath   = "/mcp"
	jsonRPCVersion = "2.0"
	jsonRPCServer  = -32000
)

// errNoBox answers a box its grant did not create as the resolver answers another user's box.
var errNoBox = status.Error(codes.NotFound, "no such box for this user (it may have been deleted)")

type jsonRPCError struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Error   struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Grants checks the OAuth access tokens of agents and lists the boxes each grant created.
type Grants interface {
	Authenticate(ctx context.Context, header string) (oauth.Grant, error)
	Boxes(ctx context.Context, grant string) ([]string, error)
	Challenge(invalid bool) string
}

// route is where one MCP exchange goes and as whom: the caller for the log, and the grant of a share call.
type route struct {
	caller string
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

// EnableGrants admits an agent's OAuth access token on the boxes its grant created, and on nothing else.
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

// route authorizes one exchange for the bearer's user, or, when no user holds it and it is a share secret, for that share.
func (m *MCP) route(r *http.Request, boxID string) (rt route, err error) {
	authorization := r.Header.Get("Authorization")
	if m.grants != nil && oauth.IsAccessToken(authorization) {
		return m.grantRoute(r, authorization, boxID)
	}
	owner, err := m.authn.Authenticate(r.Context(), authorization)
	if secret, ok := auth.ShareSecret(authorization); err != nil && ok && boxID != "" {
		var guest string
		if guest, err = m.authn.Guest(r.Context(), r.Header.Get(auth.HeaderGuestToken)); err != nil {
			return rt, err
		}
		var grant auth.Grant
		if grant, err = m.authn.Admit(secret, boxID, guest); err != nil {
			return rt, err
		}
		rt.grant, owner = &grant, grant.Owner
		rt.caller = grant.Principal()
	} else {
		if err != nil {
			return rt, err
		}
		rt.caller = owner
	}
	if boxID == "" {
		return rt, status.Error(codes.InvalidArgument, "box id is required: "+MCPPrefix+"<box id>")
	}
	if rt.ep, rt.wake, err = m.resolver.Resolve(r.Context(), owner, boxID); err == nil {
		rt.left = m.resolver.Reached(boxID)
	}
	return rt, err
}

// grantRoute authorizes one exchange for an access token on a box its grant created, as the box owner.
func (m *MCP) grantRoute(r *http.Request, authorization, boxID string) (rt route, err error) {
	g, err := m.grants.Authenticate(r.Context(), authorization)
	switch {
	case errors.Is(err, oauth.ErrInvalidToken):
		return rt, status.Error(codes.Unauthenticated, "invalid access token")
	case err != nil:
		log.WithFunc("edge.MCP.grantRoute").Error(r.Context(), err, "check access token")
		return rt, status.Error(codes.Unavailable, "checking the access token failed")
	}
	rt.caller = g.User + " grant=" + g.ID
	ids, err := m.grants.Boxes(r.Context(), g.ID)
	switch {
	case err != nil:
		log.WithFunc("edge.MCP.grantRoute").Errorf(r.Context(), err, "list the boxes of grant %s", g.ID)
		return rt, status.Error(codes.Unavailable, "reading the client's boxes failed")
	case boxID == "":
		return rt, status.Error(codes.InvalidArgument, "box id is required: "+MCPPrefix+"<box id>")
	case !slices.Contains(ids, boxID):
		return rt, errNoBox
	}
	if rt.ep, rt.wake, err = m.resolver.Resolve(r.Context(), g.User, boxID); err == nil {
		rt.left = m.resolver.Reached(boxID)
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

func writeJSONRPCError(w http.ResponseWriter, err error) {
	body := jsonRPCError{JSONRPC: jsonRPCVersion}
	body.Error.Code = jsonRPCServer
	body.Error.Message = status.Convert(err).Message()
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
