package dialer

import (
	"cmp"
	"context"
	"errors"
	"io"
	"iter"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/credential"
	agentv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

func TestDialerForwardsTheCallersCredentialAndDrainsStreams(t *testing.T) {
	ep := machines.TCPEndpoint(serveState(t, &fakeState{}))
	pool := NewPool()
	t.Cleanup(pool.Close)
	d := New(pool)
	ctx := metadata.AppendToOutgoingContext(t.Context(), credential.Header, "cred-1")

	plan, err := d.Plan(ctx, "cb-1", ep, &agentv1.PlanRequest{})
	if err != nil || plan.GetSnapshotId() != "s1" {
		t.Fatalf("Plan = %v, %v", plan, err)
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
	pool := NewPool()
	t.Cleanup(pool.Close)
	d := New(pool)
	ctx := metadata.AppendToOutgoingContext(t.Context(), credential.Header, "cred-1")
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
	pool := NewPool()
	t.Cleanup(pool.Close)
	d := New(pool)
	if _, err := d.Plan(metadata.AppendToOutgoingContext(t.Context(), credential.Header, "cred-1"), "cb-1", ep, &agentv1.PlanRequest{}); err != nil {
		t.Errorf("Plan through an endpoint whose key resolves nowhere: %v", err)
	}
}

func TestDialerAddsNoCredentialOfItsOwn(t *testing.T) {
	ep := machines.TCPEndpoint(serveState(t, &fakeState{}))
	pool := NewPool()
	t.Cleanup(pool.Close)
	d := New(pool)
	if _, err := d.Plan(t.Context(), "cb-1", ep, &agentv1.PlanRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Plan without a credential: %v, want Unauthenticated", err)
	}
}

func serveState(t *testing.T, srv agentv1.StateServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	agentv1.RegisterStateServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

// fakeState requires credential cred-1 and no edge token; Upload sends two progress messages and, with the manifest, a final one, as the agent does; Restore finishes only after it received some gets.
type fakeState struct {
	agentv1.UnimplementedStateServer
}

func (fakeState) Plan(ctx context.Context, _ *agentv1.PlanRequest) (*agentv1.PlanResponse, error) {
	if err := checkCredential(ctx); err != nil {
		return nil, err
	}
	return &agentv1.PlanResponse{SnapshotId: "s1"}, nil
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

func pages(fail error, reqs ...*agentv1.RestoreRequest) iter.Seq2[*agentv1.RestoreRequest, error] {
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

func checkCredential(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(credential.Header); len(v) != 1 || v[0] != "cred-1" || len(md.Get("x-cocoonbox-edge-token")) != 0 {
		return status.Error(codes.Unauthenticated, "bad credential")
	}
	return nil
}
