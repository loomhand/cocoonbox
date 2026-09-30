// Package edge forwards cocoonbox.agent.v1 calls to the caller's box without decoding them.
package edge

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
)

const (
	// HeaderBox names the target box of an agent call.
	HeaderBox = "x-cocoonbox-box"
	// HeaderEdgeToken carries the shared secret box agents require from the edge.
	HeaderEdgeToken = "x-cocoonbox-edge-token" //nolint:gosec // a header name, not a credential
	// MaxMessageSize bounds one proxied frame in either direction.
	MaxMessageSize = 64 << 20

	agentServicePrefix = "/cocoonbox.agent.v1."
	stateServicePrefix = "/cocoonbox.agent.v1.State/"
	agentIdleTimeout   = time.Minute
)

var (
	droppedHeaders = []string{auth.HeaderAuthorization, HeaderEdgeToken, "content-type", "user-agent", "te"}

	// agentConnectParams caps redial backoff at 1s so an agent that starts after a refused dial is reachable at once.
	agentConnectParams = grpc.ConnectParams{
		Backoff:           backoff.Config{BaseDelay: 100 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: time.Second},
		MinConnectTimeout: 5 * time.Second,
	}
)

// Resolver maps a caller and box id to the box agent's address.
type Resolver interface {
	Resolve(ctx context.Context, user, boxID string) (string, error)
}

// Proxy is the unknown-service handler that forwards agent calls frame by frame.
type Proxy struct {
	resolver  Resolver
	edgeToken string
	codec     Codec

	mu    sync.Mutex
	conns map[string]*boxConn
}

// NewProxy builds a Proxy that authenticates to agents with edgeToken; an empty edgeToken refuses every call.
func NewProxy(resolver Resolver, edgeToken string) *Proxy {
	return &Proxy{resolver: resolver, edgeToken: edgeToken, codec: NewCodec(), conns: map[string]*boxConn{}}
}

// Handle proxies one call; install it with grpc.UnknownServiceHandler.
func (p *Proxy) Handle(_ any, ss grpc.ServerStream) error {
	ctx := ss.Context()
	method, ok := grpc.MethodFromServerStream(ss)
	if !ok || !strings.HasPrefix(method, agentServicePrefix) {
		return status.Errorf(codes.Unimplemented, "unknown method %s", method)
	}
	if strings.HasPrefix(method, stateServicePrefix) {
		return status.Errorf(codes.PermissionDenied, "%s is internal to the control plane", method)
	}
	user := auth.User(ctx)
	if user == "" {
		return status.Error(codes.Unauthenticated, "unauthenticated")
	}
	if p.edgeToken == "" {
		return status.Error(codes.Unavailable, "agent edge is not configured")
	}
	md, _ := metadata.FromIncomingContext(ctx)
	boxes := md.Get(HeaderBox)
	if len(boxes) != 1 || boxes[0] == "" {
		return status.Errorf(codes.InvalidArgument, "exactly one %s header is required", HeaderBox)
	}
	addr, err := p.resolver.Resolve(ctx, user, boxes[0])
	if err != nil {
		return err
	}
	conn, err := p.conn(ctx, boxes[0], addr)
	if err != nil {
		return err
	}

	start := time.Now()
	upCtx, cancel := context.WithCancel(metadata.NewOutgoingContext(ctx, p.outgoing(md)))
	defer cancel()
	up, err := conn.NewStream(upCtx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method, grpc.ForceCodecV2(p.codec))
	if err == nil {
		err = p.pump(ctx, ss, up, cancel)
	}
	log.WithFunc("edge.Proxy.Handle").Infof(ctx, "%s user=%s box=%q addr=%s code=%s in %s",
		method, user, boxes[0], addr, status.Code(err), time.Since(start).Round(time.Millisecond))
	return err
}

// Close closes every agent connection.
func (p *Proxy) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, bc := range p.conns {
		_ = bc.conn.Close()
		delete(p.conns, id)
	}
}

func (p *Proxy) pump(ctx context.Context, down grpc.ServerStream, up grpc.ClientStream, cancel context.CancelFunc) error {
	sent := make(chan error, 1)
	received := make(chan error, 1)
	go func() { sent <- forwardRequests(down, up) }()
	go func() { received <- forwardResponses(up, down) }()
	for {
		select {
		case err := <-sent:
			if !errors.Is(err, io.EOF) {
				cancel()
				if ctx.Err() != nil {
					return status.FromContextError(ctx.Err()).Err()
				}
				return status.Errorf(codes.Internal, "forward request: %v", err)
			}
			if err := up.CloseSend(); err != nil {
				cancel()
				return status.Errorf(codes.Internal, "close upstream send: %v", err)
			}
			sent = nil
		case err := <-received:
			down.SetTrailer(up.Trailer())
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (p *Proxy) conn(ctx context.Context, boxID, addr string) (*grpc.ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if bc := p.conns[boxID]; bc != nil {
		if bc.addr == addr {
			return bc.conn, nil
		}
		_ = bc.conn.Close()
		delete(p.conns, boxID)
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMessageSize)),
		grpc.WithConnectParams(agentConnectParams),
		grpc.WithIdleTimeout(agentIdleTimeout))
	if err != nil {
		log.WithFunc("edge.Proxy.conn").Errorf(ctx, err, "dial box %s at %s", boxID, addr)
		return nil, status.Error(codes.Internal, "dial box")
	}
	p.conns[boxID] = &boxConn{addr: addr, conn: conn}
	return conn, nil
}

func (p *Proxy) outgoing(in metadata.MD) metadata.MD {
	out := metadata.MD{}
	for k, v := range in {
		if strings.HasPrefix(k, ":") || strings.HasPrefix(k, "grpc-") {
			continue
		}
		out[k] = v
	}
	for _, k := range droppedHeaders {
		delete(out, k)
	}
	out.Set(HeaderEdgeToken, p.edgeToken)
	return out
}

type boxConn struct {
	addr string
	conn *grpc.ClientConn
}

// forwardRequests copies client frames upstream until the client half-closes (io.EOF) or fails.
func forwardRequests(down grpc.ServerStream, up grpc.ClientStream) error {
	for {
		f := &frame{}
		if err := down.RecvMsg(f); err != nil {
			return err
		}
		if err := up.SendMsg(f); err != nil {
			return err
		}
	}
}

// forwardResponses copies agent headers and frames downstream; io.EOF means the agent finished with OK.
func forwardResponses(up grpc.ClientStream, down grpc.ServerStream) error {
	if header, err := up.Header(); err == nil && header != nil {
		if err := down.SendHeader(header); err != nil {
			return err
		}
	}
	for {
		f := &frame{}
		if err := up.RecvMsg(f); err != nil {
			return err
		}
		if err := down.SendMsg(f); err != nil {
			return err
		}
	}
}
