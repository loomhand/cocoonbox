// Package dialer holds the connections to box agents and calls their State service for the control plane.
package dialer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"

	"google.golang.org/grpc"

	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

// ProgressFunc receives each progress message of a transfer.
type ProgressFunc func(*agentv1.TransferProgress)

type restoreCall func(agentv1.StateClient, context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[agentv1.RestoreRequest, agentv1.TransferProgress], error)

// Dialer calls the agents' State service over pool's connections.
type Dialer struct {
	pool *Pool
}

// New returns a Dialer; callers put the box credential in each call's context.
func New(pool *Pool) *Dialer {
	return &Dialer{pool: pool}
}

// Plan calls State.Plan on the agent at ep.
func (d *Dialer) Plan(ctx context.Context, box string, ep machines.Endpoint, req *agentv1.PlanRequest) (*agentv1.PlanResponse, error) {
	client, err := d.state(box, ep)
	if err != nil {
		return nil, err
	}
	return client.Plan(ctx, req)
}

// Upload calls State.Upload and returns the final progress message.
func (d *Dialer) Upload(ctx context.Context, box string, ep machines.Endpoint, req *agentv1.UploadRequest, progress ProgressFunc) (*agentv1.TransferProgress, error) {
	client, err := d.state(box, ep)
	if err != nil {
		return nil, err
	}
	stream, err := client.Upload(ctx, req)
	if err != nil {
		return nil, err
	}
	return drain(stream, progress, req.GetManifest() != nil)
}

// Restore sends reqs as the request stream of State.Restore and returns the final progress message; an error from reqs ends the call with it.
func (d *Dialer) Restore(ctx context.Context, box string, ep machines.Endpoint, reqs iter.Seq2[*agentv1.RestoreRequest, error], progress ProgressFunc) (*agentv1.TransferProgress, error) {
	return d.restore(ctx, box, ep, agentv1.StateClient.Restore, reqs, progress)
}

// RestorePaths sends reqs as the request stream of State.RestorePaths, like Restore.
func (d *Dialer) RestorePaths(ctx context.Context, box string, ep machines.Endpoint, reqs iter.Seq2[*agentv1.RestoreRequest, error], progress ProgressFunc) (*agentv1.TransferProgress, error) {
	return d.restore(ctx, box, ep, agentv1.StateClient.RestorePaths, reqs, progress)
}

func (d *Dialer) restore(ctx context.Context, box string, ep machines.Endpoint, open restoreCall, reqs iter.Seq2[*agentv1.RestoreRequest, error], progress ProgressFunc) (*agentv1.TransferProgress, error) {
	client, err := d.state(box, ep)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := open(client, ctx)
	if err != nil {
		return nil, err
	}
	sent := make(chan error, 1)
	go func() { sent <- send(stream, reqs, cancel) }()
	final, err := drain(stream, progress, true)
	cancel()
	if sendErr := <-sent; sendErr != nil {
		return nil, sendErr
	}
	return final, err
}

func (d *Dialer) state(box string, ep machines.Endpoint) (agentv1.StateClient, error) {
	conn, err := d.pool.Conn(box, ep)
	if err != nil {
		return nil, fmt.Errorf("dial agent %s: %w", ep.Key, err)
	}
	return agentv1.NewStateClient(conn), nil
}

// send writes reqs to stream and half-closes it; a failed Send leaves the reason to Recv.
func send(stream grpc.BidiStreamingClient[agentv1.RestoreRequest, agentv1.TransferProgress], reqs iter.Seq2[*agentv1.RestoreRequest, error], cancel context.CancelFunc) error {
	for req, err := range reqs {
		if err != nil {
			cancel()
			return err
		}
		if stream.Send(req) != nil {
			return nil
		}
	}
	return stream.CloseSend()
}

func drain(stream grpc.ClientStream, progress ProgressFunc, final bool) (*agentv1.TransferProgress, error) {
	var last *agentv1.TransferProgress
	for {
		msg := &agentv1.TransferProgress{}
		err := stream.RecvMsg(msg)
		if errors.Is(err, io.EOF) {
			if final && (last == nil || !last.GetFinished()) {
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
