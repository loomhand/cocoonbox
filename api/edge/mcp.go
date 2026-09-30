package edge

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/credential"
	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	// MCPPrefix starts every remote-tools path: MCPPrefix + <box id> [+ subpath].
	MCPPrefix = "/cocoonbox.mcp/"

	agentMCPPath   = "/mcp"
	jsonRPCVersion = "2.0"
	jsonRPCServer  = -32000
)

type jsonRPCError struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Error   struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
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

	mu         sync.Mutex
	transports map[string]*boxTransport
}

// NewMCP builds the MCP proxy that authenticates to agents with box credentials from creds.
func NewMCP(resolver Resolver, authn *auth.Authenticator, creds Credentials) *MCP {
	return &MCP{resolver: resolver, authn: authn, creds: creds, transports: map[string]*boxTransport{}}
}

func (m *MCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, MCPPrefix) {
		http.NotFound(w, r)
		return
	}
	start := time.Now()
	boxID, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, MCPPrefix), "/")
	user, ep, wake, err := m.route(r.Context(), r.Header.Get("Authorization"), boxID)
	defer func() {
		log.WithFunc("edge.MCP.ServeHTTP").Infof(r.Context(), "%s %q user=%s box=%q addr=%s code=%s in %s",
			r.Method, r.URL.Path, user, boxID, ep.Key, status.Code(err), time.Since(start).Round(time.Millisecond))
	}()
	if err != nil {
		writeJSONRPCError(w, err)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { m.rewrite(pr, ep.Key, boxID, wake) },
		Transport:     m.transportFor(boxID, ep),
		FlushInterval: -1,
		ErrorHandler:  m.upstreamError,
	}
	proxy.ServeHTTP(w, r)
}

func (m *MCP) route(ctx context.Context, authorization, boxID string) (user string, ep machines.Endpoint, wake string, err error) {
	if user, err = m.authn.Authenticate(authorization); err != nil {
		return "", ep, "", err
	}
	if boxID == "" {
		return user, ep, "", status.Error(codes.InvalidArgument, "box id is required: "+MCPPrefix+"<box id>")
	}
	ep, wake, err = m.resolver.Resolve(ctx, user, boxID)
	return user, ep, wake, err
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

func (m *MCP) rewrite(pr *httputil.ProxyRequest, addr, box, wake string) {
	_, rest, _ := strings.Cut(strings.TrimPrefix(pr.In.URL.Path, MCPPrefix), "/")
	pr.Out.URL.Scheme = "http"
	pr.Out.URL.Host = addr
	pr.Out.URL.Path = agentMCPPath
	if rest != "" {
		pr.Out.URL.Path += "/" + rest
	}
	pr.Out.URL.RawPath = ""
	pr.Out.Host = pr.Out.URL.Host
	pr.Out.Header.Del("Authorization")
	pr.Out.Header.Del(credential.Header)
	pr.Out.Header.Del(HeaderEdgeToken)
	if wake != "" {
		pr.Out.Header.Set(credential.Header, m.creds.Credential(box, wake))
	}
}

func (m *MCP) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	log.WithFunc("edge.MCP.upstreamError").Warnf(r.Context(), "proxy %s to box agent: %v", r.URL.Path, err)
	writeJSONRPCError(w, status.Error(codes.Unavailable, "box agent unreachable"))
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
