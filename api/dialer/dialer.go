// Package dialer calls box agents on behalf of the control plane.
package dialer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
)

const (
	// HeaderEdgeToken carries the shared secret box agents require.
	HeaderEdgeToken = "x-cocoonbox-edge-token" //nolint:gosec // a header name, not a credential

	idleTimeout    = time.Minute
	maxMessageSize = 64 << 20
)

var connectParams = grpc.ConnectParams{
	Backoff:           backoff.Config{BaseDelay: 100 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: time.Second},
	MinConnectTimeout: 5 * time.Second,
}

// ProgressFunc receives each progress message of a transfer.
type ProgressFunc func(*agentv1.TransferProgress)

// Dialer keeps one connection per agent address.
type Dialer struct {
	edgeToken string

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

// New authenticates to agents with edgeToken.
func New(edgeToken string) *Dialer {
	return &Dialer{edgeToken: edgeToken, conns: map[string]*grpc.ClientConn{}}
}

// Plan calls State.Plan on the agent at addr.
func (d *Dialer) Plan(ctx context.Context, addr string, req *agentv1.PlanRequest) (*agentv1.PlanResponse, error) {
	client, err := d.state(addr)
	if err != nil {
		return nil, err
	}
	return client.Plan(d.outgoing(ctx), req)
}

// Upload calls State.Upload and returns the final progress message.
func (d *Dialer) Upload(ctx context.Context, addr string, req *agentv1.UploadRequest, progress ProgressFunc) (*agentv1.TransferProgress, error) {
	client, err := d.state(addr)
	if err != nil {
		return nil, err
	}
	stream, err := client.Upload(d.outgoing(ctx), req)
	if err != nil {
		return nil, err
	}
	return drain(stream, progress)
}

// Restore calls State.Restore and returns the final progress message.
func (d *Dialer) Restore(ctx context.Context, addr string, req *agentv1.RestoreRequest, progress ProgressFunc) (*agentv1.TransferProgress, error) {
	client, err := d.state(addr)
	if err != nil {
		return nil, err
	}
	stream, err := client.Restore(d.outgoing(ctx), req)
	if err != nil {
		return nil, err
	}
	return drain(stream, progress)
}

// Close closes every connection.
func (d *Dialer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for addr, conn := range d.conns {
		_ = conn.Close()
		delete(d.conns, addr)
	}
}

func (d *Dialer) state(addr string) (agentv1.StateClient, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if conn := d.conns[addr]; conn != nil {
		return agentv1.NewStateClient(conn), nil
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageSize), grpc.MaxCallSendMsgSize(maxMessageSize)),
		grpc.WithConnectParams(connectParams),
		grpc.WithIdleTimeout(idleTimeout))
	if err != nil {
		return nil, fmt.Errorf("dial agent %s: %w", addr, err)
	}
	d.conns[addr] = conn
	return agentv1.NewStateClient(conn), nil
}

func (d *Dialer) outgoing(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, HeaderEdgeToken, d.edgeToken)
}

func drain(stream grpc.ServerStreamingClient[agentv1.TransferProgress], progress ProgressFunc) (*agentv1.TransferProgress, error) {
	var last *agentv1.TransferProgress
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if last == nil || !last.GetFinished() {
				return nil, errors.New("agent ended the transfer without a final message")
			}
			return last, nil
		}
		if err != nil {
			return nil, err
		}
		last = msg
		if progress != nil {
			progress(msg)
		}
	}
}
