package dialer

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/loomhand/cocoonbox/api/credential"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	bigPlanPage  = 2048
	bigPlanPages = 500
)

func TestDialerForwardsTheCallersCredentialAndDrainsStreams(t *testing.T) {
	ep := machines.TCPEndpoint(serveState(t, &fakeState{}))
	d := newDialer(t)
	ctx := credCtx(t)

	var plan []*agentv1.PlanResponse
	if err := d.Plan(ctx, "cb-1", ep, &agentv1.PlanRequest{}, collect(&plan)); err != nil || len(plan) != 2 || plan[0].GetSnapshotId() != "s1" || len(plan[1].GetChunks()) != 1 {
		t.Fatalf("Plan = %v, %v", plan, err)
	}
	stop := errors.New("stop")
	if err := d.Plan(ctx, "cb-1", ep, &agentv1.PlanRequest{}, func(*agentv1.PlanResponse) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("Plan with a failing page = %v, want %v", err, stop)
	}
	var seen int
	final, err := d.Upload(ctx, "cb-1", ep, &agentv1.UploadRequest{SnapshotId: "s1", Manifest: &agentv1.PresignedPut{ChunkId: "s1"}}, func(*agentv1.TransferProgress) { seen++ })
	if err != nil || !final.GetFinished() || final.GetManifestId() != "s1" || seen != 3 {
		t.Errorf("Upload = %v, %v after %d messages", final, err, seen)
	}
	if batch, err := d.Upload(ctx, "cb-1", ep, &agentv1.UploadRequest{SnapshotId: "s1"}, nil); err != nil || batch.GetFinished() {
		t.Errorf("Upload of a batch without the manifest = %v, %v", batch, err)
	}
	if _, err := d.Restore(ctx, "cb-1", ep, pages(nil, &agentv1.RestoreRequest{}), nil); err == nil {
		t.Error("Restore without a final message succeeded")
	}
}

func TestDialerStreamsRestorePagesAndEndsOnAPageError(t *testing.T) {
	ep := machines.TCPEndpoint(serveState(t, &fakeState{}))
	d := newDialer(t)
	ctx := credCtx(t)
	reqs := []*agentv1.RestoreRequest{
		{Manifest: &agentv1.PresignedGet{ChunkId: "m"}},
		{Gets: []*agentv1.PresignedGet{{ChunkId: "a"}, {ChunkId: "b"}}},
		{Gets: []*agentv1.PresignedGet{{ChunkId: "c"}}},
	}
	final, err := d.Restore(ctx, "cb-1", ep, pages(nil, reqs...), nil)
	if err != nil || !final.GetFinished() || final.GetDoneChunks() != 3 || final.GetManifestId() != "m" {
		t.Errorf("Restore of three pages = %v, %v", final, err)
	}
	presign := errors.New("presign failed")
	if _, err := d.Restore(ctx, "cb-1", ep, pages(presign, reqs...), nil); !errors.Is(err, presign) {
		t.Errorf("Restore with a failing page = %v, want %v", err, presign)
	}
}

func TestDialerDialsTheEndpointNotItsKey(t *testing.T) {
	ep := machines.TCPEndpoint(serveState(t, &fakeState{}))
	ep.Key = "box.invalid:1"
	if err := newDialer(t).Plan(credCtx(t), "cb-1", ep, &agentv1.PlanRequest{}, collect(new([]*agentv1.PlanResponse))); err != nil {
		t.Errorf("Plan through an endpoint whose key resolves nowhere: %v", err)
	}
}

func TestDialerAddsNoCredentialOfItsOwn(t *testing.T) {
	ep := machines.TCPEndpoint(serveState(t, &fakeState{}))
	if err := newDialer(t).Plan(t.Context(), "cb-1", ep, &agentv1.PlanRequest{}, collect(new([]*agentv1.PlanResponse))); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Plan without a credential: %v, want Unauthenticated", err)
	}
}

func TestPlanFallsBackToUnaryPlanOnAnAgentWithoutPlanPages(t *testing.T) {
	ep := machines.TCPEndpoint(serveState(t, unaryPlan{}))
	var plan []*agentv1.PlanResponse
	if err := newDialer(t).Plan(t.Context(), "cb-1", ep, &agentv1.PlanRequest{}, collect(&plan)); err != nil || len(plan) != 1 || len(plan[0].GetChunks()) != 2 {
		t.Errorf("Plan from an agent without PlanPages = %v, %v", plan, err)
	}
}

func TestPlanTakesAChunkListLargerThanOneMessage(t *testing.T) {
	ep := machines.TCPEndpoint(serveState(t, bigPlan{}))
	var chunks, bytes int
	err := newDialer(t).Plan(t.Context(), "cb-1", ep, &agentv1.PlanRequest{}, func(page *agentv1.PlanResponse) error {
		chunks += len(page.GetChunks())
		bytes += proto.Size(page)
		return nil
	})
	if err != nil || chunks != bigPlanPages*bigPlanPage || bytes <= MaxMessageSize {
		t.Errorf("Plan = %d chunks in %d bytes, %v; want %d chunks past %d bytes", chunks, bytes, err, bigPlanPages*bigPlanPage, MaxMessageSize)
	}
}

func serve(t *testing.T, register func(*grpc.Server)) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	register(s)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

func serveState(t *testing.T, srv agentv1.StateServer) string {
	t.Helper()
	return serve(t, func(s *grpc.Server) { agentv1.RegisterStateServer(s, srv) })
}

func newDialer(t *testing.T) *Dialer {
	pool := NewPool()
	t.Cleanup(pool.Close)
	return New(pool)
}

func credCtx(t *testing.T) context.Context {
	return metadata.AppendToOutgoingContext(t.Context(), credential.Header, "cred-1")
}

type fakeState struct {
	agentv1.UnimplementedStateServer
}

func (fakeState) PlanPages(_ *agentv1.PlanRequest, stream grpc.ServerStreamingServer[agentv1.PlanResponse]) error {
	if err := checkCredential(stream.Context()); err != nil {
		return err
	}
	if err := stream.Send(&agentv1.PlanResponse{SnapshotId: "s1", Chunks: []*agentv1.ChunkRef{{Id: "a"}}}); err != nil {
		return err
	}
	return stream.Send(&agentv1.PlanResponse{Chunks: []*agentv1.ChunkRef{{Id: "b"}}})
}

func (fakeState) Upload(req *agentv1.UploadRequest, stream grpc.ServerStreamingServer[agentv1.TransferProgress]) error {
	if err := checkCredential(stream.Context()); err != nil {
		return err
	}
	msgs := []*agentv1.TransferProgress{{DoneChunks: 1}, {DoneChunks: 2}}
	if req.GetManifest() != nil {
		msgs = append(msgs, &agentv1.TransferProgress{Finished: true, ManifestId: req.GetSnapshotId()})
	}
	for _, p := range msgs {
		if err := stream.Send(p); err != nil {
			return err
		}
	}
	return nil
}

func (fakeState) Restore(stream grpc.BidiStreamingServer[agentv1.RestoreRequest, agentv1.TransferProgress]) error {
	var manifest string
	var gets uint32
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		manifest = cmp.Or(manifest, req.GetManifest().GetChunkId())
		gets += uint32(len(req.GetGets()))
	}
	return stream.Send(&agentv1.TransferProgress{DoneChunks: gets, Finished: gets > 0, ManifestId: manifest})
}

type unaryPlan struct {
	agentv1.UnimplementedStateServer
}

func (unaryPlan) Plan(context.Context, *agentv1.PlanRequest) (*agentv1.PlanResponse, error) {
	return &agentv1.PlanResponse{SnapshotId: "s1", Chunks: []*agentv1.ChunkRef{{Id: "a"}, {Id: "b"}}}, nil
}

type bigPlan struct {
	agentv1.UnimplementedStateServer
}

func (bigPlan) PlanPages(_ *agentv1.PlanRequest, stream grpc.ServerStreamingServer[agentv1.PlanResponse]) error {
	id := strings.Repeat("a", 64)
	for range bigPlanPages {
		page := &agentv1.PlanResponse{SnapshotId: id, Chunks: make([]*agentv1.ChunkRef, bigPlanPage)}
		for i := range page.Chunks {
			page.Chunks[i] = &agentv1.ChunkRef{Id: id, Size: 4 << 20}
		}
		if err := stream.Send(page); err != nil {
			return err
		}
	}
	return nil
}

func pages(fail error, reqs ...*agentv1.RestoreRequest) RestoreRequests {
	return func(yield func(*agentv1.RestoreRequest, error) bool) {
		for _, req := range reqs {
			if !yield(req, nil) {
				return
			}
		}
		if fail != nil {
			yield(nil, fail)
		}
	}
}

func collect(into *[]*agentv1.PlanResponse) PlanPageFunc {
	return func(page *agentv1.PlanResponse) error {
		*into = append(*into, page)
		return nil
	}
}

func checkCredential(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(credential.Header); len(v) != 1 || v[0] != "cred-1" || len(md.Get("x-cocoonbox-edge-token")) != 0 {
		return status.Error(codes.Unauthenticated, "bad credential")
	}
	return nil
}
