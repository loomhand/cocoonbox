package dialer

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/credential"
	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

func TestDialerForwardsTheCallersCredentialAndDrainsStreams(t *testing.T) {
	ep := machines.TCPEndpoint(serveState(t, &fakeState{}))
	d := New()
	t.Cleanup(d.Close)
	ctx := metadata.AppendToOutgoingContext(t.Context(), credential.Header, "cred-1")

	plan, err := d.Plan(ctx, ep, &agentv1.PlanRequest{})
	if err != nil || plan.GetSnapshotId() != "s1" {
		t.Fatalf("Plan = %v, %v", plan, err)
	}
	var seen int
	final, err := d.Upload(ctx, ep, &agentv1.UploadRequest{SnapshotId: "s1", Manifest: &agentv1.PresignedPut{ChunkId: "s1"}}, func(*agentv1.TransferProgress) { seen++ })
	if err != nil || !final.GetFinished() || final.GetManifestId() != "s1" || seen != 3 {
		t.Errorf("Upload = %v, %v after %d messages", final, err, seen)
	}
	if batch, err := d.Upload(ctx, ep, &agentv1.UploadRequest{SnapshotId: "s1"}, nil); err != nil || batch.GetFinished() {
		t.Errorf("Upload of a batch without the manifest = %v, %v", batch, err)
	}
	if _, err := d.Restore(ctx, ep, &agentv1.RestoreRequest{}, nil); err == nil {
		t.Error("Restore without a final message succeeded")
	}
}

func TestDialerDialsTheEndpointNotItsKey(t *testing.T) {
	ep := machines.TCPEndpoint(serveState(t, &fakeState{}))
	ep.Key = "box.invalid:1"
	d := New()
	t.Cleanup(d.Close)
	if _, err := d.Plan(metadata.AppendToOutgoingContext(t.Context(), credential.Header, "cred-1"), ep, &agentv1.PlanRequest{}); err != nil {
		t.Errorf("Plan through an endpoint whose key resolves nowhere: %v", err)
	}
}

func TestDialerAddsNoCredentialOfItsOwn(t *testing.T) {
	ep := machines.TCPEndpoint(serveState(t, &fakeState{}))
	d := New()
	t.Cleanup(d.Close)
	if _, err := d.Plan(t.Context(), ep, &agentv1.PlanRequest{}); status.Code(err) != codes.Unauthenticated {
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

// fakeState requires credential cred-1 and no edge token; Upload sends two progress messages and, with the manifest, a final one, as the agent does; Restore ends without a final message.
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

func (fakeState) Restore(_ *agentv1.RestoreRequest, stream grpc.ServerStreamingServer[agentv1.TransferProgress]) error {
	return stream.Send(&agentv1.TransferProgress{DoneChunks: 1})
}

func checkCredential(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(credential.Header); len(v) != 1 || v[0] != "cred-1" || len(md.Get("x-cocoonbox-edge-token")) != 0 {
		return status.Error(codes.Unauthenticated, "bad credential")
	}
	return nil
}
