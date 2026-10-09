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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/boxes"
	"github.com/CMGS/cocoonbox/api/credential"
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
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
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
}

func TestMCPMetersScriptRunCalls(t *testing.T) {
	env := newMCPEnv(t)
	path := filepath.Join(t.TempDir(), "cua-meter.jsonl")
	meter, err := OpenMeter(path)
	if err != nil {
		t.Fatalf("open meter: %v", err)
	}
	env.mcp.EnableMeter(meter)
	tests := []struct {
		name, body, reply string
		want              []meterRow
	}{
		{
			name:  "a call",
			body:  `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"script_run","arguments":{"code":"1"}}}`,
			reply: `{"id":7,"jsonrpc":"2.0","result":{"content":[],"isError":false}}`,
			want:  []meterRow{{Tool: "script_run", OK: true, BytesOut: 64}},
		},
		{
			name:  "a failed call in a batch",
			body:  `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shell"}},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"script_run"}}]`,
			reply: `[{"id":1,"jsonrpc":"2.0","result":{"isError":false}},{"id":2,"jsonrpc":"2.0","result":{"isError":true}}]`,
			want:  []meterRow{{Tool: "script_run", BytesOut: 50}},
		},
		{
			name:  "a duplicated method name the agent reads last",
			body:  `{"jsonrpc":"2.0","id":3,"method":"ping","method":"tools/call","params":{"name":"script_run"}}`,
			reply: `{"id":3,"jsonrpc":"2.0","error":{"code":-32603,"message":"x"}}`,
			want:  []meterRow{{Tool: "script_run", BytesOut: 62}},
		},
		{
			name:  "an escaped name",
			body:  `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"simul\u0061ng_run"}}`,
			reply: `{"id":5,"jsonrpc":"2.0","result":{"isError":false}}`,
			want:  []meterRow{{Tool: "script_run", OK: true, BytesOut: 51}},
		},
		{
			name:  "another tool",
			body:  `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"shell"}}`,
			reply: `{"id":4,"jsonrpc":"2.0","result":{"isError":false}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := meterRows(t, path)
			env.upstream.reply.Store(&tt.reply)
			resp := env.do(t, env.request(t, http.MethodPost, boxes.BoxID("alice"), tt.body))
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != tt.reply {
				t.Fatalf("client got %q, want the agent's reply", body)
			}
			got := meterRows(t, path)[len(before):]
			if len(got) != len(tt.want) {
				t.Fatalf("rows %+v, want %d", got, len(tt.want))
			}
			for i, w := range tt.want {
				g := got[i]
				if g.Tool != w.Tool || g.OK != w.OK || g.BytesOut != w.BytesOut || g.UID != "alice" || g.GWUser != "alice" || g.Machine != boxes.BoxID("alice") || g.Primitives == nil {
					t.Errorf("row %+v, want %+v for alice's box", g, w)
				}
			}
		})
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
	served   chan string
	addr     string
	mcp      *MCP
	resolver *fakeResolver
	upstream *fakeMCP
	shares   *fakeShares
	users    staticUsers
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
	authn := auth.New(users, shares)
	served := make(chan string, 16)
	mcp := NewMCP(resolver, authn, fakeCreds{})
	addr := serveHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { served <- r.Method }()
		mcp.ServeHTTP(w, r)
	}))
	return &mcpEnv{served: served, addr: addr, mcp: mcp, resolver: resolver, upstream: upstream, shares: shares, users: users, client: &http.Client{Transport: newH2CTransport((&net.Dialer{Timeout: 5 * time.Second}).DialContext)}}
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

func meterRows(t *testing.T, path string) []meterRow {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read meter: %v", err)
	}
	var rows []meterRow
	for line := range strings.Lines(string(raw)) {
		var row meterRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("meter line %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
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

// fakeMCP answers POST with reply when set, else with what it received, and GET with one SSE event per signal on next.
type fakeMCP struct {
	reply  atomic.Pointer[string]
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
	if reply := f.reply.Load(); reply != nil {
		_, _ = io.WriteString(w, *reply)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{
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
