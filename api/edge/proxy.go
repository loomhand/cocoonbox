// Package edge forwards cocoonbox.agent.v1 calls to the caller's box without decoding them; it serves Session.Screen from the box's RFB server.
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/credential"
	"github.com/loomhand/cocoonbox/api/dialer"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	// HeaderBox names the target box of an agent call.
	HeaderBox = "x-cocoonbox-box"

	agentServicePrefix = "/cocoonbox.agent.v1."
	stateServicePrefix = "/cocoonbox.agent.v1.State/"
	// sessionEvents only watches the box, so it keeps no box up.
	sessionEvents = "/cocoonbox.agent.v1.Session/Events"
	sessionScreen = "/cocoonbox.agent.v1.Session/Screen"
	// maxScreensPerBox bounds the RFB relays one box holds open, owner and share streams together.
	maxScreensPerBox = 8
)

var droppedHeaders = []string{auth.HeaderAuthorization, credential.Header, "content-type", "user-agent", "te"}

// Resolver maps a caller and box id to the box agent's endpoint and the wake id its credential names.
type Resolver interface {
	Resolve(ctx context.Context, user, boxID string) (ep machines.Endpoint, wake string, err error)
	// Reached records that a client's call goes to boxID and returns what records its end; the api's own calls do not report.
	Reached(boxID string) func()
}

// Credentials issues the credentials an agent accepts for a box at a wake: the owner's, or one naming a share's principal.
type Credentials interface {
	Credential(box, wake string) string
	ShareCredential(box, wake, principal string) string
}

// Proxy is the unknown-service handler that forwards agent calls frame by frame.
type Proxy struct {
	resolver Resolver
	creds    Credentials
	codec    Codec
	pool     *dialer.Pool

	screensMu sync.Mutex
	screens   map[string]int
}

// NewProxy builds a Proxy that reaches agents over pool and authenticates to them with box credentials from creds.
func NewProxy(resolver Resolver, creds Credentials, pool *dialer.Pool) *Proxy {
	return &Proxy{resolver: resolver, creds: creds, codec: NewCodec(), pool: pool, screens: map[string]int{}}
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
	user, err := auth.Caller(ctx)
	if err != nil {
		return err
	}
	md, _ := metadata.FromIncomingContext(ctx)
	boxes := md.Get(HeaderBox)
	if len(boxes) != 1 || boxes[0] == "" {
		return status.Errorf(codes.InvalidArgument, "exactly one %s header is required", HeaderBox)
	}
	ep, wake, err := p.resolver.Resolve(ctx, user, boxes[0])
	if err != nil {
		return err
	}
	if method != sessionEvents {
		defer p.resolver.Reached(boxes[0])()
	}

	start := time.Now()
	if method == sessionScreen {
		err = p.relayScreen(ctx, ss, boxes[0], ep)
	} else {
		err = p.forward(ctx, ss, method, boxes[0], p.outgoing(md, boxes[0], wake), ep)
	}
	log.WithFunc("edge.Proxy.Handle").Infof(ctx, "%s user=%s box=%q addr=%s code=%s in %s",
		method, user, boxes[0], ep.Key, status.Code(err), time.Since(start).Round(time.Millisecond))
	return err
}

// EndShare asks the box agent to close every MCP session of share id.
func (p *Proxy) EndShare(ctx context.Context, owner, box, id string) error {
	ep, wake, err := p.resolver.Resolve(ctx, owner, box)
	if err != nil {
		return err
	}
	conn, err := p.conn(ctx, box, ep)
	if err != nil {
		return err
	}
	if wake != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, credential.Header, p.creds.Credential(box, wake))
	}
	_, err = agentv1.NewAgentsClient(conn).End(ctx, &agentv1.EndRequest{Target: &agentv1.EndRequest_ShareId{ShareId: id}})
	return err
}

func (p *Proxy) forward(ctx context.Context, down grpc.ServerStream, method, box string, md metadata.MD, ep machines.Endpoint) error {
	conn, err := p.conn(ctx, box, ep)
	if err != nil {
		return err
	}
	upCtx, cancel := context.WithCancel(metadata.NewOutgoingContext(ctx, md))
	defer cancel()
	up, err := conn.NewStream(upCtx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method, grpc.ForceCodecV2(p.codec))
	if err != nil {
		log.WithFunc("edge.Proxy.forward").Errorf(ctx, err, "open a stream to box %s", box)
		return errUnreachable
	}
	return pump(ctx, down, up, cancel)
}

func (p *Proxy) conn(ctx context.Context, boxID string, ep machines.Endpoint) (*grpc.ClientConn, error) {
	conn, err := p.pool.Conn(boxID, ep)
	if err != nil {
		log.WithFunc("edge.Proxy.conn").Errorf(ctx, err, "dial box %s at %s", boxID, ep.Key)
		return nil, status.Error(codes.Internal, "dial box")
	}
	return conn, nil
}

func (p *Proxy) outgoing(in metadata.MD, box, wake string) metadata.MD {
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
	if wake != "" {
		out.Set(credential.Header, p.creds.Credential(box, wake))
	}
	return out
}

func pump(ctx context.Context, down grpc.ServerStream, up grpc.ClientStream, cancel context.CancelFunc) error {
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
