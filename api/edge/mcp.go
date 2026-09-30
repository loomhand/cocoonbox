package edge

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
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

// MCP proxies MCP Streamable HTTP exchanges to the agent of the caller's box.
type MCP struct {
	resolver  Resolver
	authn     *auth.Authenticator
	edgeToken string
	transport *http.Transport
}

// NewMCP builds the MCP proxy; an empty edgeToken refuses every exchange.
func NewMCP(resolver Resolver, authn *auth.Authenticator, edgeToken string) *MCP {
	return &MCP{resolver: resolver, authn: authn, edgeToken: edgeToken, transport: newH2CTransport()}
}

func (m *MCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	boxID, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, MCPPrefix), "/")
	user, addr, err := m.route(r.Context(), r.Header.Get("Authorization"), boxID)
	if err != nil {
		writeJSONRPCError(w, err)
	} else {
		proxy := &httputil.ReverseProxy{
			Rewrite:       func(pr *httputil.ProxyRequest) { m.rewrite(pr, addr) },
			Transport:     m.transport,
			FlushInterval: -1,
			ErrorHandler:  m.upstreamError,
		}
		proxy.ServeHTTP(w, r)
	}
	log.WithFunc("edge.MCP.ServeHTTP").Infof(r.Context(), "%s %s user=%s box=%s addr=%s code=%s in %s",
		r.Method, r.URL.Path, user, boxID, addr, status.Code(err), time.Since(start).Round(time.Millisecond))
}

func (m *MCP) route(ctx context.Context, authorization, boxID string) (string, string, error) {
	user, err := m.authn.Authenticate(authorization)
	if err != nil {
		return "", "", err
	}
	if m.edgeToken == "" {
		return user, "", status.Error(codes.Unavailable, "agent edge is not configured")
	}
	if boxID == "" {
		return user, "", status.Error(codes.InvalidArgument, "box id is required: "+MCPPrefix+"<box id>")
	}
	addr, err := m.resolver.Resolve(ctx, user, boxID)
	return user, addr, err
}

func (m *MCP) rewrite(pr *httputil.ProxyRequest, addr string) {
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
	pr.Out.Header.Set(HeaderEdgeToken, m.edgeToken)
}

func (m *MCP) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	log.WithFunc("edge.MCP.upstreamError").Warnf(r.Context(), "proxy %s to box agent: %v", r.URL.Path, err)
	writeJSONRPCError(w, status.Error(codes.Unavailable, "box agent unreachable"))
}

// NewHandler serves gRPC and MCP on one HTTP/2 listener, dispatching on the gRPC content type.
func NewHandler(grpcServer http.Handler, mcp http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc"):
			grpcServer.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, MCPPrefix):
			mcp.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func newH2CTransport() *http.Transport {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	return &http.Transport{
		Protocols:       &protocols,
		DialContext:     (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		IdleConnTimeout: agentIdleTimeout,
	}
}

func writeJSONRPCError(w http.ResponseWriter, err error) {
	body := jsonRPCError{JSONRPC: jsonRPCVersion}
	body.Error.Code = jsonRPCServer
	body.Error.Message = status.Convert(err).Message()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus(status.Code(err)))
	_ = json.NewEncoder(w).Encode(body)
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
