// Package dialer holds the connections to box agents and calls their State service for the control plane.
package dialer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

// ProgressFunc receives each progress message of a transfer.
type ProgressFunc func(*agentv1.TransferProgress)

type RestoreRequests = iter.Seq2[*agentv1.RestoreRequest, error]

// PlanPageFunc receives each message of a plan: the first carries the snapshot, every one a page of its chunks.
type PlanPageFunc func(*agentv1.PlanResponse) error

type restoreCall func(agentv1.StateClient, context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[agentv1.RestoreRequest, agentv1.TransferProgress], error)

// Dialer calls the agents' State service over pool's connections.
type Dialer struct {
	pool *Pool
}

// New returns a Dialer; callers put the box credential in each call's context.
func New(pool *Pool) *Dialer {
	return &Dialer{pool: pool}
}

// Plan calls State.PlanPages on the agent at ep, or State.Plan on an agent without it, and hands each message to page; an error from page ends the call with it.
func (d *Dialer) Plan(ctx context.Context, box string, ep machines.Endpoint, req *agentv1.PlanRequest, page PlanPageFunc) error {
	client, err := d.state(box, ep)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.PlanPages(ctx, req)
	if err != nil {
		return err
	}
	for first := true; ; first = false {
		msg, err := stream.Recv()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case first && status.Code(err) == codes.Unimplemented:
			plan, planErr := client.Plan(ctx, req)
			if planErr != nil {
				return planErr
			}
			return page(plan)
		case err != nil:
			return err
		}
		if err := page(msg); err != nil {
			return err
		}
	}
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
func (d *Dialer) Restore(ctx context.Context, box string, ep machines.Endpoint, reqs RestoreRequests, progress ProgressFunc) (*agentv1.TransferProgress, error) {
	return d.restore(ctx, box, ep, agentv1.StateClient.Restore, reqs, progress)
}

// RestorePaths sends reqs as the request stream of State.RestorePaths, like Restore.
func (d *Dialer) RestorePaths(ctx context.Context, box string, ep machines.Endpoint, reqs RestoreRequests, progress ProgressFunc) (*agentv1.TransferProgress, error) {
	return d.restore(ctx, box, ep, agentv1.StateClient.RestorePaths, reqs, progress)
}

func (d *Dialer) restore(ctx context.Context, box string, ep machines.Endpoint, open restoreCall, reqs RestoreRequests, progress ProgressFunc) (*agentv1.TransferProgress, error) {
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
func send(stream grpc.BidiStreamingClient[agentv1.RestoreRequest, agentv1.TransferProgress], reqs RestoreRequests, cancel context.CancelFunc) error {
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
