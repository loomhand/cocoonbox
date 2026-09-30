package edge

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
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

func TestMCPPostRoundTrip(t *testing.T) {
	env := newMCPEnv(t)
	req := env.request(t, http.MethodPost, boxes.BoxID("alice")+"/extra?x=1", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req.Header.Set("Mcp-Session-Id", "s0")
	req.Header.Set(HeaderEdgeToken, "forged")
	resp := env.do(t, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK || resp.Header.Get("Mcp-Session-Id") != "sess-1" {
		t.Fatalf("status %d, Mcp-Session-Id %q", resp.StatusCode, resp.Header.Get("Mcp-Session-Id"))
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{
		"path": "/mcp/extra", "query": "x=1", "session": "s0", "edge_token": edgeToken, "authorization": "",
		"body": `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("upstream saw %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestMCPStreamsEvents(t *testing.T) {
	env := newMCPEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	req := env.request(t, http.MethodGet, boxes.BoxID("alice"), "").WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	resp := env.do(t, req)
	defer resp.Body.Close()
	lines := bufio.NewReader(resp.Body)

	for i := range 3 {
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("read event %d: %v", i, err)
		}
		if want := "data: " + string(rune('0'+i)) + "\n"; line != want {
			t.Fatalf("event %d = %q, want %q", i, line, want)
		}
		if _, err := lines.ReadString('\n'); err != nil {
			t.Fatalf("read event %d terminator: %v", i, err)
		}
		env.upstream.next <- struct{}{}
	}
	cancel()
	select {
	case <-env.upstream.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream stream was not cancelled")
	}
}

func TestMCPRejects(t *testing.T) {
	env := newMCPEnv(t)
	tests := []struct {
		name, authorization, box string
		resolveErr               error
		want                     int
		message                  string
	}{
		{name: "no token", box: boxes.BoxID("alice"), want: http.StatusUnauthorized, message: "missing bearer token"},
		{name: "bad token", authorization: "Bearer wrong", box: boxes.BoxID("alice"), want: http.StatusUnauthorized, message: "invalid bearer token"},
		{name: "someone else's box", authorization: "Bearer " + aliceToken, box: boxes.BoxID("bob"), want: http.StatusForbidden, message: "box belongs to another user"},
		{name: "no box id", authorization: "Bearer " + aliceToken, want: http.StatusBadRequest, message: "box id is required: /cocoonbox.mcp/<box id>"},
		{
			name: "box not running", authorization: "Bearer " + aliceToken, box: boxes.BoxID("alice"),
			resolveErr: status.Error(codes.Unavailable, "box is BOX_STATE_HIBERNATED"), want: http.StatusServiceUnavailable, message: "box is BOX_STATE_HIBERNATED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env.resolver.err = tt.resolveErr
			req := env.request(t, http.MethodPost, tt.box, `{}`)
			req.Header.Set("Authorization", tt.authorization)
			resp := env.do(t, req)
			defer resp.Body.Close()
			var body jsonRPCError
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.StatusCode != tt.want || body.JSONRPC != "2.0" || body.Error.Code != -32000 || body.Error.Message != tt.message {
				t.Errorf("got %d %+v, want %d %q", resp.StatusCode, body, tt.want, tt.message)
			}
		})
	}
	if n := env.upstream.calls.Load(); n != 0 {
		t.Errorf("upstream received %d rejected exchanges", n)
	}
}

func TestMCPAgentUnreachable(t *testing.T) {
	env := newMCPEnv(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	env.resolver.addr = lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	resp := env.do(t, env.request(t, http.MethodPost, boxes.BoxID("alice"), `{}`))
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "box agent unreachable") {
		t.Errorf("got %d %s", resp.StatusCode, body)
	}
}

func TestHandlerServesGRPCBesideMCP(t *testing.T) {
	env := newMCPEnv(t)
	conn, err := grpc.NewClient(env.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx := metadata.AppendToOutgoingContext(t.Context(), auth.HeaderAuthorization, "Bearer "+aliceToken, HeaderBox, boxes.BoxID("alice"))
	resp, err := agentv1.NewSessionClient(conn).Hello(ctx, &agentv1.HelloRequest{})
	if err != nil || resp.GetAgentVersion() != edgeToken {
		t.Errorf("Hello = %v, %v", resp, err)
	}
	stream, err := agentv1.NewSessionClient(conn).Screen(ctx)
	if err != nil {
		t.Fatalf("Screen: %v", err)
	}
	if err := stream.Send(&agentv1.ScreenClientFrame{Body: &agentv1.ScreenClientFrame_Data{Data: []byte("x")}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got, err := stream.Recv(); err != nil || string(got.GetData()) != "x" {
		t.Errorf("Screen echo = %v, %v", got, err)
	}
}

type mcpEnv struct {
	addr     string
	resolver *fakeResolver
	upstream *fakeMCP
	client   *http.Client
}

// newMCPEnv serves a fake box agent (gRPC Session plus /mcp on one h2c listener, like cocoonbox-agent) behind the edge handler.
func newMCPEnv(t *testing.T) *mcpEnv {
	t.Helper()
	upstream := &fakeMCP{next: make(chan struct{}), closed: make(chan struct{})}
	agentSrv := grpc.NewServer()
	agentv1.RegisterSessionServer(agentSrv, &fakeAgent{screenDone: make(chan error, 1)})
	resolver := &fakeResolver{addr: serveHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			agentSrv.ServeHTTP(w, r)
			return
		}
		upstream.ServeHTTP(w, r)
	}))}

	sum := sha256.Sum256([]byte(aliceToken))
	authn := auth.New([]config.User{{Name: "alice", TokenSHA256: hex.EncodeToString(sum[:])}})
	proxy := NewProxy(resolver, edgeToken)
	t.Cleanup(proxy.Close)
	edgeSrv := grpc.NewServer(
		grpc.ForceServerCodecV2(NewCodec()),
		grpc.UnknownServiceHandler(proxy.Handle),
		grpc.ChainUnaryInterceptor(authn.Unary),
		grpc.ChainStreamInterceptor(authn.Stream),
	)
	addr := serveHTTP(t, NewHandler(edgeSrv, NewMCP(resolver, authn, edgeToken)))
	return &mcpEnv{addr: addr, resolver: resolver, upstream: upstream, client: &http.Client{Transport: newH2CTransport()}}
}

func (e *mcpEnv) request(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+e.addr+MCPPrefix+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func (e *mcpEnv) do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	return resp
}

func serveHTTP(t *testing.T, handler http.Handler) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: handler, Protocols: &protocols, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return lis.Addr().String()
}

// fakeMCP answers POST with what it received and GET with one SSE event per signal on next.
type fakeMCP struct {
	calls  atomic.Int64
	next   chan struct{}
	closed chan struct{}
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if r.Header.Get(HeaderEdgeToken) != edgeToken {
		http.Error(w, "bad edge token", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet {
		f.stream(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Mcp-Session-Id", "sess-1")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"path": r.URL.Path, "query": r.URL.RawQuery, "session": r.Header.Get("Mcp-Session-Id"),
		"edge_token": r.Header.Get(HeaderEdgeToken), "authorization": r.Header.Get("Authorization"), "body": string(body),
	})
}

func (f *fakeMCP) stream(w http.ResponseWriter, r *http.Request) {
	defer close(f.closed)
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	for i := 0; ; i++ {
		if _, err := io.WriteString(w, "data: "+string(rune('0'+i%10))+"\n\n"); err != nil {
			return
		}
		flusher.Flush()
		select {
		case <-f.next:
		case <-r.Context().Done():
			return
		}
	}
}
