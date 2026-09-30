package dialer

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
)

func TestDialerSendsEdgeTokenAndDrainsStreams(t *testing.T) {
	addr := serveState(t, &fakeState{})
	d := New("edge-secret")
	t.Cleanup(d.Close)

	plan, err := d.Plan(t.Context(), addr, &agentv1.PlanRequest{})
	if err != nil || plan.GetSnapshotId() != "s1" {
		t.Fatalf("Plan = %v, %v", plan, err)
	}
	var seen int
	final, err := d.Upload(t.Context(), addr, &agentv1.UploadRequest{SnapshotId: "s1"}, func(*agentv1.TransferProgress) { seen++ })
	if err != nil || !final.GetFinished() || final.GetManifestId() != "s1" || seen != 3 {
		t.Errorf("Upload = %v, %v after %d messages", final, err, seen)
	}
	if _, err := d.Restore(t.Context(), addr, &agentv1.RestoreRequest{}, nil); err == nil {
		t.Error("Restore without a final message succeeded")
	}
}

func TestDialerWithoutTokenIsRejected(t *testing.T) {
	addr := serveState(t, &fakeState{})
	d := New("wrong")
	t.Cleanup(d.Close)
	if _, err := d.Plan(t.Context(), addr, &agentv1.PlanRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Plan with a wrong token: %v, want Unauthenticated", err)
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

// fakeState requires edge-secret; Upload sends two progress messages and a final one, Restore ends without a final message.
type fakeState struct {
	agentv1.UnimplementedStateServer
}

func (fakeState) Plan(ctx context.Context, _ *agentv1.PlanRequest) (*agentv1.PlanResponse, error) {
	if err := checkToken(ctx); err != nil {
		return nil, err
	}
	return &agentv1.PlanResponse{SnapshotId: "s1"}, nil
}

func (fakeState) Upload(req *agentv1.UploadRequest, stream grpc.ServerStreamingServer[agentv1.TransferProgress]) error {
	if err := checkToken(stream.Context()); err != nil {
		return err
	}
	for _, p := range []*agentv1.TransferProgress{{DoneChunks: 1}, {DoneChunks: 2}, {Finished: true, ManifestId: req.GetSnapshotId()}} {
		if err := stream.Send(p); err != nil {
			return err
		}
	}
	return nil
}

func (fakeState) Restore(_ *agentv1.RestoreRequest, stream grpc.ServerStreamingServer[agentv1.TransferProgress]) error {
	return stream.Send(&agentv1.TransferProgress{DoneChunks: 1})
}

func checkToken(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(HeaderEdgeToken); len(v) != 1 || v[0] != "edge-secret" {
		return status.Error(codes.Unauthenticated, "bad edge token")
	}
	return nil
}
