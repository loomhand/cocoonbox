package edge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/boxes"
	agentv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/agent/v1"
)

const (
	bobToken     = "bob-secret"
	carolToken   = "carol-secret"
	openSecret   = auth.SecretPrefix + "0123456789abcdef0123456789abcdef"
	namedSecret  = auth.SecretPrefix + "fedcba9876543210fedcba9876543210"
	staleSecret  = auth.SecretPrefix + "00000000000000000000000000000000"
	toolsListRPC = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
)

func TestMCPAuthorizesOwnerOrShare(t *testing.T) {
	env := newMCPEnv(t)
	env.resolver.wake = "w1"
	box := boxes.BoxID("alice")
	env.shares.put(openSecret, auth.Share{ID: "sh-open", Owner: "alice", Box: box, Write: true})
	env.shares.put(namedSecret, auth.Share{ID: "sh-named", Owner: "alice", Box: box, Users: []string{"bob"}})
	env.shares.put(staleSecret, auth.Share{ID: "sh-stale", Owner: "alice", Box: box, ExpiresAt: time.Now().Add(-time.Second)})
	tests := []struct {
		name, authorization, guest, box string
		want                            int
		credential                      string
	}{
		{name: "owner", authorization: "Bearer " + aliceToken, box: box, want: http.StatusOK, credential: "cred:" + box + ":w1"},
		{name: "open share", authorization: "Bearer " + openSecret, box: box, want: http.StatusOK, credential: "cred:" + box + ":w1:share:sh-open:write"},
		{name: "open share ignores a guest token", authorization: "Bearer " + openSecret, guest: carolToken, box: box, want: http.StatusOK, credential: "cred:" + box + ":w1:share:sh-open:write"},
		{name: "named share, named user", authorization: "Bearer " + namedSecret, guest: bobToken, box: box, want: http.StatusOK, credential: "cred:" + box + ":w1:share:sh-named:read:bob"},
		{name: "named share without a token", authorization: "Bearer " + namedSecret, box: box, want: http.StatusUnauthorized},
		{name: "named share, another user", authorization: "Bearer " + namedSecret, guest: carolToken, box: box, want: http.StatusForbidden},
		{name: "named share, bad token", authorization: "Bearer " + namedSecret, guest: "nope", box: box, want: http.StatusUnauthorized},
		{name: "expired share", authorization: "Bearer " + staleSecret, box: box, want: http.StatusForbidden},
		{name: "revoked or regenerated share", authorization: "Bearer " + auth.SecretPrefix + "gone", box: box, want: http.StatusForbidden},
		{name: "share of another box", authorization: "Bearer " + openSecret, box: boxes.BoxID("bob"), want: http.StatusForbidden},
		{name: "the share owner's guest token is no owner token", authorization: "Bearer " + namedSecret, guest: aliceToken, box: box, want: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := env.request(t, http.MethodPost, tt.box, toolsListRPC)
			req.Header.Set("Authorization", tt.authorization)
			if tt.guest != "" {
				req.Header.Set(auth.HeaderGuestToken, tt.guest)
			}
			resp := env.do(t, req)
			defer resp.Body.Close()
			if resp.StatusCode != tt.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d %s, want %d", resp.StatusCode, body, tt.want)
			}
			if tt.want != http.StatusOK {
				return
			}
			var got map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got["credential"] != tt.credential || got["authorization"] != "" || got["guest"] != "" {
				t.Errorf("upstream saw credential %q, authorization %q, guest %q; want %q and neither header", got["credential"], got["authorization"], got["guest"], tt.credential)
			}
		})
	}
}

func TestShareStreamsEndWithTheShare(t *testing.T) {
	env := newMCPEnv(t)
	box := boxes.BoxID("alice")
	for _, end := range []string{"write switched", "share ended"} {
		cut, moded := make(chan struct{}), make(chan struct{})
		env.shares.put(openSecret, auth.Share{ID: "sh-open", Owner: "alice", Box: box, Cut: cut, Moded: moded})
		env.upstream.closed = make(chan struct{})
		req := env.request(t, http.MethodGet, box, "")
		req.Header.Set("Authorization", "Bearer "+openSecret)
		resp := env.do(t, req)
		if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || line != "data: 0\n" {
			t.Fatalf("%s: first event %q, %v", end, line, err)
		}
		if end == "share ended" {
			close(cut)
		} else {
			close(moded)
		}
		for _, done := range []<-chan struct{}{env.upstream.closed, served(env)} {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatalf("%s: the stream is still open after 1 s", end)
			}
		}
		_ = resp.Body.Close()
	}
}

func TestWriteSwitchCutsOnlyServerStreams(t *testing.T) {
	cut, moded := make(chan struct{}), make(chan struct{})
	g := &auth.Grant{Share: auth.Share{Cut: cut, Moded: moded}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { cutOnEnd(ctx, cancel, g, false); close(done) }()
	close(moded)
	select {
	case <-done:
		t.Fatal("a write switch cut a POST exchange, which may carry a running tool call")
	case <-time.After(100 * time.Millisecond):
	}
	close(cut)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the end of the share did not cut the POST exchange")
	}
}

func TestShareSecretIsNoGRPCToken(t *testing.T) {
	env := newProxyEnv(t)
	ctx := metadata.AppendToOutgoingContext(t.Context(), auth.HeaderAuthorization, "Bearer "+openSecret, HeaderBox, boxes.BoxID("alice"))
	if _, err := env.client.Hello(ctx, &agentv1.HelloRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Hello with a share secret: %v, want Unauthenticated", err)
	}
}

func TestScreenDropsViewerInput(t *testing.T) {
	env := newProxyEnv(t)
	proxy := NewProxy(env.resolver, fakeCreds{})
	t.Cleanup(proxy.Close)
	cut := make(chan struct{})
	grant := auth.Grant{Share: auth.Share{ID: "sh-open", Owner: "alice", Box: boxes.BoxID("alice"), Cut: cut}}
	viewer, client := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- proxy.Screen(t.Context(), grant, viewer) }()

	keep := func(b ...byte) []byte { return b }
	sent := [][]byte{
		[]byte("RFB 003.008\n"), keep(1), keep(0),
		keep(2, 0, 0, 1, 0, 0, 0, 7),
		keep(4, 1, 0, 0, 0, 0, 0, 0x61),
		keep(5, 1, 0, 10, 0, 20),
		append(keep(6, 0, 0, 0, 0, 0, 0, 5), "hello"...),
		keep(3, 1, 0, 0, 0, 0, 5, 0, 3, 32),
	}
	want := bytes.Join([][]byte{sent[0], keep(1), keep(1), sent[3], sent[7]}, nil)
	var got []byte
	read := make(chan error, 1)
	go func() {
		buf := make([]byte, 256)
		for len(got) < len(want) {
			n, err := client.Read(buf)
			if err != nil {
				read <- err
				return
			}
			got = append(got, buf[:n]...)
		}
		read <- nil
	}()
	for _, b := range sent {
		if _, err := client.Write(b); err != nil {
			t.Fatalf("viewer write: %v", err)
		}
	}
	select {
	case err := <-read:
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the box saw %v (%v), want %v", got, err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the box saw %v, want %v", got, want)
	}
	start := time.Now()
	close(cut)
	select {
	case <-served:
		if took := time.Since(start); took > time.Second {
			t.Errorf("the bridge ended %s after the share", took)
		}
	case <-time.After(time.Second):
		t.Fatal("the bridge outlived its share by 1 s")
	}
}

// fakeShares serves shares by the sha256 of their secret; a zero ExpiresAt means an hour from now.
type fakeShares struct {
	mu    sync.Mutex
	bySum map[string]auth.Share
}

func (f *fakeShares) ShareBySecretSHA256(sum string) (auth.Share, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sh, ok := f.bySum[sum]
	return sh, ok
}

func (f *fakeShares) put(secret string, sh auth.Share) {
	if sh.ExpiresAt.IsZero() {
		sh.ExpiresAt = time.Now().Add(time.Hour)
	}
	sum := sha256.Sum256([]byte(secret))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bySum[hex.EncodeToString(sum[:])] = sh
}

// served closes when the edge handler of the next exchange returns.
func served(env *mcpEnv) <-chan struct{} {
	done := make(chan struct{})
	go func() { <-env.served; close(done) }()
	return done
}
