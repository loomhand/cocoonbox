package edge

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/boxes"
	"github.com/loomhand/cocoonbox/api/credential"
)

func TestMCPPostRoundTrip(t *testing.T) {
	env := newMCPEnv(t)
	req := env.request(t, http.MethodPost, boxes.BoxID("alice")+"/extra?x=1", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req.Header.Set("Mcp-Session-Id", "s0")
	req.Header.Set(HeaderEdgeToken, "forged")
	req.Header.Set(credential.Header, "forged")
	env.resolver.wake = "w1"
	resp := env.do(t, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK || resp.Header.Get("Mcp-Session-Id") != "sess-1" {
		t.Fatalf("status %d, Mcp-Session-Id %q", resp.StatusCode, resp.Header.Get("Mcp-Session-Id"))
	}
	var got map[string]string
	if err := json.UnmarshalRead(resp.Body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{
		"path": "/mcp/extra", "query": "x=1", "session": "s0", "edge_token": "", "authorization": "",
		"credential": "cred:" + boxes.BoxID("alice") + ":w1",
		"body":       `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("upstream saw %s = %q, want %q", k, got[k], v)
		}
	}
	if reached := env.resolver.reaches(); !slices.Equal(reached, []string{boxes.BoxID("alice")}) {
		t.Errorf("reached %v, want alice's box", reached)
	}
}

func TestMCPDialsTheEndpointNotItsKey(t *testing.T) {
	env := newMCPEnv(t)
	env.resolver.opaque = true
	resp := env.do(t, env.request(t, http.MethodPost, boxes.BoxID("alice"), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d through an endpoint whose key resolves nowhere, want 200", resp.StatusCode)
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
	select {
	case method := <-env.served:
		if method != http.MethodGet {
			t.Errorf("edge finished %s, want GET", method)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("edge handler did not return after the client cancelled")
	}
	if reached := env.resolver.reaches(); len(reached) != 0 {
		t.Errorf("a GET event stream reached %v; it only watches the box", reached)
	}
}

func TestMCPStreamEndsWhenItsUserIsDisabled(t *testing.T) {
	env := newMCPEnv(t)
	req := env.request(t, http.MethodGet, boxes.BoxID("alice"), "")
	req.Header.Set("Accept", "text/event-stream")
	resp := env.do(t, req)
	defer resp.Body.Close()
	if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || line != "data: 0\n" {
		t.Fatalf("first event = %q, %v", line, err)
	}
	env.disable()
	select {
	case <-env.upstream.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its user")
	}
}

func TestMCPStreamEndsWhenItsTokenIsRevoked(t *testing.T) {
	env := newMCPEnv(t)
	req := env.request(t, http.MethodGet, boxes.BoxID("alice"), "")
	req.Header.Set("Accept", "text/event-stream")
	resp := env.do(t, req)
	defer resp.Body.Close()
	if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || line != "data: 0\n" {
		t.Fatalf("first event = %q, %v", line, err)
	}
	env.revoke()
	select {
	case <-env.upstream.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its token")
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
		{name: "someone else's box", authorization: "Bearer " + aliceToken, box: boxes.BoxID("bob"), want: http.StatusNotFound, message: "no such box for this user (it may have been deleted)"},
		{name: "no box id", authorization: "Bearer " + aliceToken, want: http.StatusBadRequest, message: "box id is required: /cocoonbox.mcp/<box id>"},
		{
			name: "box not running", authorization: "Bearer " + aliceToken, box: boxes.BoxID("alice"),
			resolveErr: status.Error(codes.Unavailable, "box is hibernated; call box_ensure to bring it up"), want: http.StatusServiceUnavailable, message: "box is hibernated; call box_ensure to bring it up",
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
			if err := json.UnmarshalRead(resp.Body, &body); err != nil {
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

func TestMCPExchangeSendsTheOwnersCredential(t *testing.T) {
	env := newMCPEnv(t)
	env.resolver.wake = "w1"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://unset/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set(credential.Header, "forged")
	var got map[string]string
	err = env.mcp.Exchange(req, "alice", boxes.BoxID("alice"), func(resp *http.Response) error {
		return json.UnmarshalRead(resp.Body, &got)
	})
	if err != nil || got["path"] != "/mcp" || got["credential"] != "cred:"+boxes.BoxID("alice")+":w1" || got["body"] != `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` {
		t.Fatalf("exchange = %v, upstream saw %v", err, got)
	}
	if reached := env.resolver.reaches(); !slices.Equal(reached, []string{boxes.BoxID("alice")}) {
		t.Errorf("reached %v, want alice's box", reached)
	}

	env.resolver.err = status.Error(codes.Unavailable, "box is hibernated; call box_ensure to bring it up")
	used := false
	err = env.mcp.Exchange(req.Clone(t.Context()), "alice", boxes.BoxID("alice"), func(*http.Response) error { used = true; return nil })
	if status.Code(err) != codes.Unavailable || used {
		t.Fatalf("exchange with a sleeping box = %v, used %t", err, used)
	}
	if err = env.mcp.Exchange(req.Clone(t.Context()), "alice", boxes.BoxID("bob"), func(*http.Response) error { return nil }); status.Code(err) != codes.NotFound {
		t.Fatalf("exchange with someone else's box = %v", err)
	}

	env.resolver.err = nil
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	env.resolver.addr = lis.Addr().String()
	if err = lis.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err = env.mcp.Exchange(req.Clone(t.Context()), "alice", boxes.BoxID("alice"), func(*http.Response) error { used = true; return nil }); status.Code(err) != codes.Unknown || used {
		t.Fatalf("exchange with an agent that does not answer = %v, used %t", err, used)
	}
}

func TestMCPIgnoresOtherPaths(t *testing.T) {
	env := newMCPEnv(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+env.addr+"/cocoonbox.agent.v1.Session/Hello", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp := env.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}

type mcpEnv struct {
	mcp      *MCP
	served   chan string
	addr     string
	resolver *fakeResolver
	upstream *fakeMCP
	shares   *fakeShares
	users    staticUsers
	disable  context.CancelFunc
	revoke   context.CancelFunc
	client   *http.Client
}

// newMCPEnv serves a fake agent /mcp over HTTP/1.1 and h2c behind the MCP proxy.
func newMCPEnv(t *testing.T) *mcpEnv {
	t.Helper()
	upstream := &fakeMCP{next: make(chan struct{}), closed: make(chan struct{})}
	resolver := &fakeResolver{addr: serveHTTP(t, upstream)}

	sum := sha256.Sum256([]byte(aliceToken))
	bobSum, carolSum := sha256.Sum256([]byte(bobToken)), sha256.Sum256([]byte(carolToken))
	shares := &fakeShares{bySum: map[string]auth.Share{}}
	users := staticUsers{hex.EncodeToString(sum[:]): "alice", hex.EncodeToString(bobSum[:]): "bob", hex.EncodeToString(carolSum[:]): "carol"}
	life, disable := context.WithCancel(t.Context())
	aliceLife, revoke := context.WithCancel(life)
	authn := auth.New(livingUsers{staticUsers: users, life: life, credentials: map[string]context.Context{hex.EncodeToString(sum[:]): aliceLife}}, shares)
	served := make(chan string, 16)
	mcp := NewMCP(resolver, authn, fakeCreds{})
	addr := serveHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { served <- r.Method }()
		mcp.ServeHTTP(w, r)
	}))
	return &mcpEnv{mcp: mcp, served: served, addr: addr, resolver: resolver, upstream: upstream, shares: shares, users: users, disable: disable, revoke: revoke, client: &http.Client{Transport: newH2CTransport((&net.Dialer{Timeout: 5 * time.Second}).DialContext)}}
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
type livingUsers struct {
	staticUsers
	life        context.Context
	credentials map[string]context.Context
}

func (u livingUsers) Lifetime(_, credential string) (context.Context, func()) {
	if life, ok := u.credentials[credential]; ok {
		return life, func() {}
	}
	return u.life, func() {}
}

type fakeMCP struct {
	calls  atomic.Int64
	next   chan struct{}
	closed chan struct{}
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if r.Method == http.MethodGet {
		f.stream(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Mcp-Session-Id", "sess-1")
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, map[string]string{
		"path": r.URL.Path, "query": r.URL.RawQuery, "session": r.Header.Get("Mcp-Session-Id"),
		"edge_token": r.Header.Get(HeaderEdgeToken), "authorization": r.Header.Get("Authorization"), "body": string(body),
		"credential": r.Header.Get(credential.Header), "guest": r.Header.Get(auth.HeaderGuestToken),
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
