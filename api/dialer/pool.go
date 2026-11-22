package dialer

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	// MaxMessageSize bounds one agent message in either direction.
	MaxMessageSize = 64 << 20

	idleTimeout = time.Minute
)

// connectParams caps redial backoff at 1s so an agent that starts after a refused dial is reachable at once.
var connectParams = grpc.ConnectParams{
	Backoff:           backoff.Config{BaseDelay: 100 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: time.Second},
	MinConnectTimeout: 5 * time.Second,
}

type agentConn struct {
	conn *grpc.ClientConn
	used time.Time
}

// identityWatch intercepts the calls of box's connection: it reports the box when a call ends with the agent's refusal for want of a valid identity file.
type identityWatch struct {
	box  string
	lost func(box string)
}

func (w identityWatch) unary(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	err := invoker(ctx, method, req, reply, cc, opts...)
	w.check(err)
	return err
}

func (w identityWatch) stream(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	s, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		w.check(err)
		return nil, err
	}
	return watchedStream{ClientStream: s, watch: w}, nil
}

func (w identityWatch) check(err error) {
	if status.Code(err) == codes.Unavailable && strings.HasPrefix(status.Convert(err).Message(), "identity ") {
		w.lost(w.box)
	}
}

type watchedStream struct {
	grpc.ClientStream
	watch identityWatch
}

func (s watchedStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil && !errors.Is(err, io.EOF) {
		s.watch.check(err)
	}
	return err
}

// Pool keeps one connection per box and agent endpoint, for the edge and the control plane alike.
type Pool struct {
	now          func() time.Time
	identityLost func(box string)

	mu    sync.Mutex
	conns map[string]map[string]*agentConn
}

// NewPool returns an empty Pool.
func NewPool() *Pool {
	return &Pool{now: time.Now, identityLost: func(string) {}, conns: map[string]map[string]*agentConn{}}
}

// OnIdentityLost calls fn with the box of each call its agent refuses for want of its identity file; set it before the first Conn.
func (p *Pool) OnIdentityLost(fn func(box string)) {
	p.identityLost = fn
}

// Conn returns box's connection to its agent at ep; it closes the box's connections to other endpoints that stay unused.
func (p *Pool) Conn(box string, ep machines.Endpoint) (*grpc.ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	conns := p.conns[box]
	if c := conns[ep.Key]; c != nil {
		c.used = now
		return c.conn, nil
	}
	for key, c := range conns {
		if s := c.conn.GetState(); s != connectivity.Ready && s != connectivity.Connecting && now.Sub(c.used) > idleTimeout {
			_ = c.conn.Close()
			delete(conns, key)
		}
	}
	watch := identityWatch{box: box, lost: p.identityLost}
	conn, err := grpc.NewClient("passthrough:///"+ep.Key,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ep.Dial(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMessageSize), grpc.MaxCallSendMsgSize(MaxMessageSize)),
		grpc.WithConnectParams(connectParams),
		grpc.WithIdleTimeout(idleTimeout),
		grpc.WithChainUnaryInterceptor(watch.unary),
		grpc.WithChainStreamInterceptor(watch.stream))
	if err != nil {
		return nil, err
	}
	if conns == nil {
		conns = map[string]*agentConn{}
		p.conns[box] = conns
	}
	conns[ep.Key] = &agentConn{conn: conn, used: now}
	return conn, nil
}

// Close closes every connection.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conns := range p.conns {
		for _, c := range conns {
			_ = c.conn.Close()
		}
	}
	clear(p.conns)
}
