package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/boxes"
	"github.com/CMGS/cocoonbox/api/config"
	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
)

const (
	aliceToken = "alice-secret"
	edgeToken  = "edge-secret"
)

func TestUnaryRewritesHeaders(t *testing.T) {
	env := newProxyEnv(t)
	var header, trailer metadata.MD
	ctx := metadata.AppendToOutgoingContext(env.aliceCtx(t), HeaderEdgeToken, "forged")
	resp, err := env.client.Hello(ctx, &agentv1.HelloRequest{ClientVersion: "test"}, grpc.Header(&header), grpc.Trailer(&trailer))
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if resp.GetAgentVersion() != edgeToken {
		t.Errorf("agent saw edge token %q, want %q", resp.GetAgentVersion(), edgeToken)
	}
	if len(resp.GetModels()) != 0 {
		t.Errorf("agent saw authorization %v, want none", resp.GetModels())
	}
	if resp.GetDefaultMainModel() != boxes.BoxID("alice") {
		t.Errorf("agent saw box %q", resp.GetDefaultMainModel())
	}
	if got := header.Get("x-agent-header"); len(got) != 1 || got[0] != "h" {
		t.Errorf("header x-agent-header = %v", got)
	}
	if got := trailer.Get("x-agent-trailer"); len(got) != 1 || got[0] != "t" {
		t.Errorf("trailer x-agent-trailer = %v", got)
	}
}

func TestStreamPassesFramesThrough(t *testing.T) {
	env := newProxyEnv(t)
	stream, err := env.client.Screen(env.aliceCtx(t))
	if err != nil {
		t.Fatalf("Screen: %v", err)
	}
	if err := stream.Send(&agentv1.ScreenClientFrame{Body: &agentv1.ScreenClientFrame_Open{Open: &agentv1.ScreenOpen{}}}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	for i := range 20 {
		want := "frame-" + strconv.Itoa(i)
		if err := stream.Send(&agentv1.ScreenClientFrame{Body: &agentv1.ScreenClientFrame_Data{Data: []byte(want)}}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		got, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv %d: %v", i, err)
		}
		if string(got.GetData()) != want {
			t.Fatalf("recv %d = %q, want %q", i, got.GetData(), want)
		}
	}
	big := make([]byte, 8<<20)
	if err := stream.Send(&agentv1.ScreenClientFrame{Body: &agentv1.ScreenClientFrame_Data{Data: big}}); err != nil {
		t.Fatalf("send 8 MiB: %v", err)
	}
	if got, err := stream.Recv(); err != nil || len(got.GetData()) != len(big) {
		t.Fatalf("recv 8 MiB: %d bytes, %v", len(got.GetData()), err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("recv after half-close: %v, want io.EOF", err)
	}
}

func TestStreamPropagatesStatus(t *testing.T) {
	env := newProxyEnv(t)
	stream, err := env.client.Screen(env.aliceCtx(t))
	if err != nil {
		t.Fatalf("Screen: %v", err)
	}
	if err := stream.Send(&agentv1.ScreenClientFrame{Body: &agentv1.ScreenClientFrame_Data{Data: []byte("fail")}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "screen refused" {
		t.Errorf("recv: %v, want FailedPrecondition screen refused", err)
	}
	if got := stream.Trailer().Get("x-agent-trailer"); len(got) != 1 || got[0] != "t" {
		t.Errorf("trailer = %v", stream.Trailer())
	}
}

func TestCancelReachesAgent(t *testing.T) {
	env := newProxyEnv(t)
	ctx, cancel := context.WithCancel(env.aliceCtx(t))
	stream, err := env.client.Screen(ctx)
	if err != nil {
		t.Fatalf("Screen: %v", err)
	}
	if err := stream.Send(&agentv1.ScreenClientFrame{Body: &agentv1.ScreenClientFrame_Data{Data: []byte("x")}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv: %v", err)
	}
	cancel()
	select {
	case err := <-env.agent.screenDone:
		if status.Code(err) != codes.Canceled {
			t.Errorf("agent stream ended with %v, want Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent stream was not cancelled")
	}
}

func TestRejectsCallers(t *testing.T) {
	env := newProxyEnv(t)
	tests := []struct {
		name string
		md   []string
		want codes.Code
	}{
		{name: "no token", md: []string{HeaderBox, boxes.BoxID("alice")}, want: codes.Unauthenticated},
		{name: "bad token", md: []string{auth.HeaderAuthorization, "Bearer wrong", HeaderBox, boxes.BoxID("alice")}, want: codes.Unauthenticated},
		{name: "someone else's box", md: []string{auth.HeaderAuthorization, "Bearer " + aliceToken, HeaderBox, boxes.BoxID("bob")}, want: codes.PermissionDenied},
		{name: "no box", md: []string{auth.HeaderAuthorization, "Bearer " + aliceToken}, want: codes.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := metadata.AppendToOutgoingContext(t.Context(), tt.md...)
			if _, err := env.client.Hello(ctx, &agentv1.HelloRequest{}); status.Code(err) != tt.want {
				t.Errorf("Hello: %v, want %s", err, tt.want)
			}
			stream, err := env.client.Screen(ctx)
			if err != nil {
				t.Fatalf("Screen: %v", err)
			}
			if _, err := stream.Recv(); status.Code(err) != tt.want {
				t.Errorf("Screen recv: %v, want %s", err, tt.want)
			}
		})
	}
	if env.agent.calls.Load() != 0 {
		t.Errorf("agent received %d rejected calls", env.agent.calls.Load())
	}
}

func TestRejectsStateMethods(t *testing.T) {
	env := newProxyEnv(t)
	_, err := agentv1.NewStateClient(env.conn).Plan(env.aliceCtx(t), &agentv1.PlanRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("State/Plan: %v, want PermissionDenied", err)
	}
	stream, err := agentv1.NewStateClient(env.conn).Restore(env.aliceCtx(t), &agentv1.RestoreRequest{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Errorf("State/Restore: %v, want PermissionDenied", err)
	}
	if env.agent.calls.Load() != 0 {
		t.Errorf("agent received %d State calls", env.agent.calls.Load())
	}
}

func TestRejectsNonAgentMethods(t *testing.T) {
	env := newProxyEnv(t)
	err := env.conn.Invoke(env.aliceCtx(t), "/grpc.health.v1.Health/Check", &agentv1.HelloRequest{}, &agentv1.HelloResponse{})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("Invoke: %v, want Unimplemented", err)
	}
}

func TestRefusesWithoutEdgeToken(t *testing.T) {
	env := newProxyEnvWithToken(t, "")
	if _, err := env.client.Hello(env.aliceCtx(t), &agentv1.HelloRequest{}); status.Code(err) != codes.Unavailable {
		t.Errorf("Hello: %v, want Unavailable", err)
	}
	if env.agent.calls.Load() != 0 {
		t.Errorf("agent received %d calls without an edge token", env.agent.calls.Load())
	}
}

func TestReachesAgentStartedAfterRefusedDial(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	env := newProxyEnvAt(t, edgeToken, addr)
	if _, err := env.client.Hello(env.aliceCtx(t), &agentv1.HelloRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("Hello before the agent listens: %v, want Unavailable", err)
	}
	time.Sleep(3 * time.Second)

	agentSrv := grpc.NewServer()
	agentv1.RegisterSessionServer(agentSrv, env.agent)
	lis, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("relisten %s: %v", addr, err)
	}
	go func() { _ = agentSrv.Serve(lis) }()
	t.Cleanup(agentSrv.Stop)

	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := env.client.Hello(env.aliceCtx(t), &agentv1.HelloRequest{})
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Hello after the agent started: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type proxyEnv struct {
	agent    *fakeAgent
	resolver *fakeResolver
	conn     *grpc.ClientConn
	client   agentv1.SessionClient
}

func newProxyEnv(t *testing.T) *proxyEnv {
	t.Helper()
	return newProxyEnvWithToken(t, edgeToken)
}

func newProxyEnvWithToken(t *testing.T, token string) *proxyEnv {
	t.Helper()
	agentSrv := grpc.NewServer(grpc.MaxRecvMsgSize(MaxMessageSize))
	env := newProxyEnvAt(t, token, "")
	agentv1.RegisterSessionServer(agentSrv, env.agent)
	env.resolver.addr = serve(t, agentSrv)
	return env
}

func newProxyEnvAt(t *testing.T, token, agentAddr string) *proxyEnv {
	t.Helper()
	agent := &fakeAgent{screenDone: make(chan error, 1)}
	resolver := &fakeResolver{addr: agentAddr}

	sum := sha256.Sum256([]byte(aliceToken))
	authn := auth.New([]config.User{{Name: "alice", TokenSHA256: hex.EncodeToString(sum[:])}})
	proxy := NewProxy(resolver, token)
	t.Cleanup(proxy.Close)
	edgeSrv := grpc.NewServer(
		grpc.ForceServerCodecV2(NewCodec()),
		grpc.UnknownServiceHandler(proxy.Handle),
		grpc.ChainUnaryInterceptor(authn.Unary),
		grpc.ChainStreamInterceptor(authn.Stream),
		grpc.MaxRecvMsgSize(MaxMessageSize),
	)
	conn, err := grpc.NewClient(serve(t, edgeSrv), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMessageSize)))
	if err != nil {
		t.Fatalf("dial edge: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &proxyEnv{agent: agent, resolver: resolver, conn: conn, client: agentv1.NewSessionClient(conn)}
}

func (e *proxyEnv) aliceCtx(t *testing.T) context.Context {
	return metadata.AppendToOutgoingContext(t.Context(),
		auth.HeaderAuthorization, "Bearer "+aliceToken, HeaderBox, boxes.BoxID("alice"))
}

func serve(t *testing.T, srv *grpc.Server) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// fakeResolver applies the Boxes ownership rule and routes every owned box to addr.
type fakeResolver struct {
	addr string
	err  error
}

func (r *fakeResolver) Resolve(_ context.Context, user, boxID string) (string, error) {
	if boxID != boxes.BoxID(user) {
		return "", status.Error(codes.PermissionDenied, "box belongs to another user")
	}
	return r.addr, r.err
}

// fakeAgent echoes what the edge delivered: Hello returns the edge token as agent_version, authorization values as models and the box header as default_main_model.
type fakeAgent struct {
	agentv1.UnimplementedSessionServer
	calls      atomic.Int64
	screenDone chan error
}

func (a *fakeAgent) Hello(ctx context.Context, _ *agentv1.HelloRequest) (*agentv1.HelloResponse, error) {
	a.calls.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	if err := grpc.SetHeader(ctx, metadata.Pairs("x-agent-header", "h")); err != nil {
		return nil, err
	}
	if err := grpc.SetTrailer(ctx, metadata.Pairs("x-agent-trailer", "t")); err != nil {
		return nil, err
	}
	return &agentv1.HelloResponse{
		AgentVersion:     first(md.Get(HeaderEdgeToken)),
		Models:           md.Get(auth.HeaderAuthorization),
		DefaultMainModel: first(md.Get(HeaderBox)),
	}, nil
}

func (a *fakeAgent) Screen(stream grpc.BidiStreamingServer[agentv1.ScreenClientFrame, agentv1.ScreenServerFrame]) error {
	a.calls.Add(1)
	err := a.screen(stream)
	a.screenDone <- err
	return err
}

func (a *fakeAgent) screen(stream grpc.BidiStreamingServer[agentv1.ScreenClientFrame, agentv1.ScreenServerFrame]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	if first(md.Get(HeaderEdgeToken)) != edgeToken {
		return status.Error(codes.Unauthenticated, "bad edge token")
	}
	for {
		in, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return status.FromContextError(stream.Context().Err()).Err()
		}
		if in.GetOpen() != nil {
			continue
		}
		if string(in.GetData()) == "fail" {
			stream.SetTrailer(metadata.Pairs("x-agent-trailer", "t"))
			return status.Error(codes.FailedPrecondition, "screen refused")
		}
		if err := stream.Send(&agentv1.ScreenServerFrame{Data: in.GetData()}); err != nil {
			return err
		}
	}
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
