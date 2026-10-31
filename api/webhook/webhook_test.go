package webhook

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/users"
)

func TestAnAdmittedTokenIsTheUserItsReviewNamesBoundToItsCredentialAtThatEntry(t *testing.T) {
	ep := newEndpoint(t)
	ep.admit("sapi_alice", "alice", "cred-alice", 60)
	accounts := &fakeAccounts{}
	hook := newHook(t, ep, accounts)
	id, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryMCP)
	if err != nil || id != (auth.Identity{User: "alice", Credential: "mcp:cred-alice"}) {
		t.Fatalf("Resolve = %+v, %v", id, err)
	}
	if accounts.ensured["alice"] != "example" {
		t.Errorf("ensured %v, want alice from example", accounts.ensured)
	}
	if got := ep.last(); got.bearer != "Bearer caller-token" || got.Token != "sapi_alice" || got.Entry != "mcp" || got.Credential != "" {
		t.Errorf("the endpoint got %+v", got)
	}
}

func TestEachAnswerHoldsForItsTTLAtItsEntryAndAnOutageNever(t *testing.T) {
	ep := newEndpoint(t)
	ep.admit("sapi_alice", "alice", "cred-alice", 60)
	hook := newHook(t, ep, &fakeAccounts{})
	for range 3 {
		if _, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryMCP); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if _, err := hook.Resolve(t.Context(), "sapi_nobody", auth.EntryMCP); !errors.Is(err, auth.ErrUnknownToken) {
			t.Fatalf("Resolve of an unknown token = %v, want ErrUnknownToken", err)
		}
	}
	if n := ep.count(); n != 2 {
		t.Errorf("the endpoint was asked %d times, want once per token", n)
	}
	if _, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryGRPC); err != nil || ep.count() != 3 {
		t.Errorf("the token at another entry = %v after %d asks, want a new ask", err, ep.count())
	}
	ep.fail(http.StatusServiceUnavailable)
	for range 2 {
		if _, err := hook.Resolve(t.Context(), "sapi_bob", auth.EntryMCP); err == nil || errors.Is(err, auth.ErrUnknownToken) {
			t.Errorf("Resolve with the endpoint failing = %v, want an outage", err)
		}
	}
	if n := ep.count(); n != 5 {
		t.Errorf("the endpoint was asked %d times, want each outage asked again", n)
	}
}

func TestAReviewWithNoTTLIsAskedAgainAndOneTooLongIsCappedByItsVerdict(t *testing.T) {
	ep := newEndpoint(t)
	ep.admit("sapi_alice", "alice", "cred-alice", 0)
	hook := newHook(t, ep, &fakeAccounts{})
	for range 2 {
		if _, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryMCP); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if n := ep.count(); n != 2 {
		t.Errorf("a review with no TTL was asked %d times, want each time", n)
	}
	for _, tc := range []struct {
		rv   review
		want time.Duration
	}{
		{review{Authenticated: true, Allowed: true, TTLSeconds: -1}, 0},
		{review{Authenticated: true, Allowed: true, TTLSeconds: 30}, 30 * time.Second},
		{review{Authenticated: true, Allowed: true, TTLSeconds: 86400}, time.Minute},
		{review{Authenticated: true, TTLSeconds: 86400}, time.Minute},
		{review{TTLSeconds: 3}, 3 * time.Second},
		{review{TTLSeconds: 60}, 5 * time.Second},
	} {
		if got := tc.rv.ttl(); got != tc.want {
			t.Errorf("ttl of %+v = %s, want %s", tc.rv, got, tc.want)
		}
	}
}

func TestTheReviewDecidesUnknownForbiddenOrBroken(t *testing.T) {
	ep := newEndpoint(t)
	ep.reviews["sapi_grpc"] = review{Authenticated: true, Allowed: false, TTLSeconds: 60}
	ep.reviews["sapi_nameless"] = review{Authenticated: true, Allowed: true, Credential: "cred", TTLSeconds: 60}
	ep.reviews["sapi_idpless"] = review{Authenticated: true, Allowed: true, User: "alice", Credential: "cred", TTLSeconds: 60}
	hook := newHook(t, ep, &fakeAccounts{})
	if _, err := hook.Resolve(t.Context(), "sapi_grpc", auth.EntryMCP); !errors.Is(err, auth.ErrNotAllowed) {
		t.Errorf("a token kept off the entry = %v, want ErrNotAllowed", err)
	}
	if _, err := hook.Resolve(t.Context(), "sapi_nobody", auth.EntryMCP); !errors.Is(err, auth.ErrUnknownToken) {
		t.Errorf("an unknown token = %v, want ErrUnknownToken", err)
	}
	if _, err := hook.Resolve(t.Context(), "sapi_nameless", auth.EntryMCP); err == nil || errors.Is(err, auth.ErrUnknownToken) {
		t.Errorf("an admission without a user = %v, want an outage", err)
	}
	for range 2 {
		if _, err := hook.Resolve(t.Context(), "sapi_idpless", auth.EntryMCP); err == nil || errors.Is(err, auth.ErrUnknownToken) {
			t.Errorf("an admission without an idp = %v, want an outage", err)
		}
	}
	if n := ep.count(); n != 5 {
		t.Errorf("the endpoint was asked %d times, want an admission without an idp asked again", n)
	}
}

func TestAReviewMissingItsVerdictIsAnOutageNotARefusal(t *testing.T) {
	ep := newEndpoint(t)
	ep.raw["sapi_empty"] = `{}`
	ep.raw["sapi_half"] = `{"authenticated":true,"user":"alice","credential":"c","ttlSeconds":60}`
	hook := newHook(t, ep, &fakeAccounts{})
	for _, token := range []string{"sapi_empty", "sapi_half"} {
		if _, err := hook.Resolve(t.Context(), token, auth.EntryMCP); err == nil || errors.Is(err, auth.ErrUnknownToken) || errors.Is(err, auth.ErrNotAllowed) {
			t.Errorf("Resolve(%s) = %v, want an outage", token, err)
		}
	}
}

func TestAReReviewMissingItsVerdictKeepsTheCalls(t *testing.T) {
	ep := newEndpoint(t)
	ep.admit("sapi_alice", "alice", "cred-alice", 1)
	accounts := &fakeAccounts{}
	hook := newHook(t, ep, accounts)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go hook.Run(ctx)
	id, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryGRPC)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer hook.Watch(id.Credential)()
	ep.mu.Lock()
	ep.rawAgain = `{}`
	ep.mu.Unlock()
	waitFor(t, func() bool { return ep.reviewed("cred-alice") > 0 })
	time.Sleep(100 * time.Millisecond)
	if ended := accounts.endedCredentials(); len(ended) != 0 {
		t.Errorf("an empty re-review ended %v", ended)
	}
}

func TestAUserCocoonboxRefusesIsAnUnknownToken(t *testing.T) {
	ep := newEndpoint(t)
	for token, user := range map[string]string{"sapi_taken": "taken", "sapi_off": "off", "sapi_bad": "bad id", "sapi_down": "down"} {
		ep.admit(token, user, "cred-"+user, 60)
	}
	hook := newHook(t, ep, &fakeAccounts{})
	for _, token := range []string{"sapi_taken", "sapi_off", "sapi_bad"} {
		if id, err := hook.Resolve(t.Context(), token, auth.EntryMCP); !errors.Is(err, auth.ErrUnknownToken) || id.User != "" {
			t.Errorf("Resolve(%s) = %+v, %v, want ErrUnknownToken", token, id, err)
		}
	}
	if _, err := hook.Resolve(t.Context(), "sapi_down", auth.EntryMCP); err == nil || errors.Is(err, auth.ErrUnknownToken) {
		t.Errorf("Resolve with the users database down = %v, want an outage", err)
	}
}

func TestAWatchedCredentialIsReviewedAgainAndEndsWhenRefused(t *testing.T) {
	ep := newEndpoint(t)
	ep.admit("sapi_alice", "alice", "cred-alice", 1)
	accounts := &fakeAccounts{}
	hook := newHook(t, ep, accounts)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go hook.Run(ctx)
	id, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryGRPC)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	release := hook.Watch(id.Credential)
	defer release()
	waitFor(t, func() bool { return ep.reviewed("cred-alice") > 0 })
	if got := ep.lastReview(); got.Token != "" || got.Entry != "grpc" {
		t.Errorf("the review again got %+v, want the credential at grpc without the token", got)
	}
	if ended := accounts.endedCredentials(); len(ended) != 0 {
		t.Fatalf("ended %v while the review still admits it", ended)
	}
	ep.revoke("cred-alice")
	waitFor(t, func() bool { return len(accounts.endedCredentials()) > 0 })
	if ended := accounts.endedCredentials(); ended[0] != "grpc:cred-alice" {
		t.Errorf("ended %v, want grpc:cred-alice", ended)
	}
	if _, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryGRPC); !errors.Is(err, auth.ErrUnknownToken) {
		t.Errorf("the token of an ended credential = %v, want it asked again and refused", err)
	}
}

func TestAHeldCredentialIsNotReviewedBeforeItsTTL(t *testing.T) {
	ep := newEndpoint(t)
	ep.admit("sapi_alice", "alice", "cred-alice", 60)
	hook := newHook(t, ep, &fakeAccounts{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go hook.Run(ctx)
	id, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryGRPC)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer hook.Watch(id.Credential)()
	time.Sleep(300 * time.Millisecond)
	if n := ep.reviewed("cred-alice"); n != 0 {
		t.Errorf("a credential a minute from its review was reviewed %d times", n)
	}
}

func TestForgettingACredentialDropsOnlyTheAnswersThatNamedIt(t *testing.T) {
	hook := newHook(t, newEndpoint(t), &fakeAccounts{})
	future := time.Now().Add(time.Minute)
	hook.remember("grpc:cred-a", "cred-a", auth.EntryGRPC, time.Minute)
	hook.answers["a"] = answer{review: review{Credential: "cred-a"}, until: future}
	hook.answers["b"] = answer{review: review{Credential: "cred-b"}, until: future}
	hook.forget("grpc:cred-a")
	if _, a := hook.answers["a"]; a || len(hook.answers) != 1 || len(hook.held) != 0 {
		t.Errorf("after forget the cache holds %v and %d held, want only b", hook.answers, len(hook.held))
	}
}

func TestAFullCacheDropsWhatExpiredBeforeKeepingMore(t *testing.T) {
	hook := newHook(t, newEndpoint(t), &fakeAccounts{})
	past, future := time.Now().Add(-time.Second), time.Now().Add(time.Minute)
	for i := range sweepAt - 1 {
		hook.answers[fmt.Sprint(i)] = answer{until: past}
	}
	hook.answers["fresh"] = answer{until: future}
	hook.keep("new", answer{until: future})
	if _, fresh := hook.answers["fresh"]; len(hook.answers) != 2 || !fresh {
		t.Errorf("after a sweep the cache holds %d answers, fresh %t, want the fresh and the new one", len(hook.answers), fresh)
	}
}

func TestACredentialNoCallHoldsIsNotReviewedAgain(t *testing.T) {
	ep := newEndpoint(t)
	ep.admit("sapi_alice", "alice", "cred-alice", 1)
	hook := newHook(t, ep, &fakeAccounts{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go hook.Run(ctx)
	id, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryGRPC)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	hook.Watch(id.Credential)()
	hook.Watch("mcp:not-ours")()
	time.Sleep(1500 * time.Millisecond)
	if n := ep.reviewed("cred-alice"); n != 0 {
		t.Errorf("a released credential was reviewed %d times", n)
	}
}

func TestABearerTheEndpointRejectsIsDroppedAndTheReviewIsAnOutage(t *testing.T) {
	ep := newEndpoint(t)
	ep.admit("sapi_alice", "alice", "cred-alice", 60)
	ep.fail(http.StatusUnauthorized)
	bearer := &fakeBearer{token: "caller-token"}
	hook, err := New(Options{URL: ep.URL, Bearer: bearer, Timeout: time.Second, Every: time.Second}, &fakeAccounts{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hook.http.Transport = ep.Client().Transport
	if _, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryMCP); err == nil || errors.Is(err, auth.ErrUnknownToken) {
		t.Errorf("Resolve with the api's bearer refused = %v, want an outage", err)
	}
	if len(bearer.rejected) != 1 || bearer.rejected[0] != "caller-token" {
		t.Errorf("rejected %v, want the bearer sent", bearer.rejected)
	}
	ep.fail(http.StatusServiceUnavailable)
	if _, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryMCP); err == nil || len(bearer.rejected) != 1 {
		t.Errorf("Resolve on a 503 = %v, rejected %v, want an outage that keeps the bearer", err, bearer.rejected)
	}
}

func TestARedirectIsAnOutageAndIsNeverFollowed(t *testing.T) {
	var leaked atomic.Bool
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		leaked.Store(true)
		_, _ = w.Write([]byte(`{"authenticated":true,"allowed":true,"idp":"example","user":"alice","credential":"c"}`))
	}))
	defer plain.Close()
	ep := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusTemporaryRedirect)
	}))
	defer ep.Close()
	hook, err := New(Options{URL: ep.URL, Bearer: &fakeBearer{token: "caller-token"}, Timeout: time.Second, Every: time.Second}, &fakeAccounts{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hook.http.Transport = ep.Client().Transport
	if _, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryMCP); err == nil || errors.Is(err, auth.ErrUnknownToken) {
		t.Errorf("Resolve through a redirect = %v, want an outage", err)
	}
	if leaked.Load() {
		t.Error("the redirect was followed to plain http")
	}
}

func TestOnlyAnHTTPSEndpointIsAsked(t *testing.T) {
	if _, err := New(Options{URL: "http://id.example/review"}, &fakeAccounts{}); err == nil {
		t.Error("New took an http endpoint")
	}
}

func newHook(t *testing.T, ep *endpoint, accounts Accounts) *Hook {
	t.Helper()
	hook, err := New(Options{URL: ep.URL, Bearer: &fakeBearer{token: "caller-token"}, Timeout: time.Second, Every: 50 * time.Millisecond}, accounts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hook.http.Transport = ep.Client().Transport
	return hook
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

type fakeBearer struct {
	mu       sync.Mutex
	token    string
	rejected []string
}

func (f *fakeBearer) Token(context.Context) (string, error) { return f.token, nil }

func (f *fakeBearer) Rejected(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, token)
}

type request struct {
	bearer     string
	Token      string `json:"token"`
	Credential string `json:"credential"`
	Entry      string `json:"entry"`
}

type endpoint struct {
	*httptest.Server
	mu       sync.Mutex
	reviews  map[string]review
	raw      map[string]string
	rawAgain string
	revoked  map[string]bool
	requests []request
	failing  int
}

func newEndpoint(t *testing.T) *endpoint {
	ep := &endpoint{reviews: map[string]review{}, raw: map[string]string{}, revoked: map[string]bool{}}
	ep.Server = httptest.NewTLSServer(http.HandlerFunc(ep.serve))
	t.Cleanup(ep.Close)
	return ep
}

func (e *endpoint) admit(token, user, credential string, ttlSeconds int) {
	e.reviews[token] = review{Authenticated: true, Allowed: true, IDP: "example", User: user, Credential: credential, Kind: "key", TTLSeconds: ttlSeconds}
}

func (e *endpoint) serve(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.UnmarshalRead(r.Body, &req); err != nil || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	req.bearer = r.Header.Get("Authorization")
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests = append(e.requests, req)
	if e.failing != 0 {
		w.WriteHeader(e.failing)
		return
	}
	if raw, ok := e.raw[req.Token]; ok && req.Credential == "" {
		_, _ = w.Write([]byte(raw))
		return
	}
	if e.rawAgain != "" && req.Credential != "" {
		_, _ = w.Write([]byte(e.rawAgain))
		return
	}
	answer := review{TTLSeconds: 5}
	if req.Credential != "" {
		answer = review{Authenticated: !e.revoked[req.Credential], Allowed: true, TTLSeconds: 1}
	} else if rv, ok := e.reviews[req.Token]; ok && !e.revoked[rv.Credential] {
		answer = rv
	}
	_ = json.MarshalWrite(w, answer)
}

func (e *endpoint) revoke(credential string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revoked[credential] = true
}

func (e *endpoint) fail(code int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failing = code
}

func (e *endpoint) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.requests)
}

func (e *endpoint) last() request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests[len(e.requests)-1]
}

func (e *endpoint) reviewed(credential string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, r := range e.requests {
		if r.Credential == credential {
			n++
		}
	}
	return n
}

func (e *endpoint) lastReview() request {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.requests) - 1; i >= 0; i-- {
		if e.requests[i].Credential != "" {
			return e.requests[i]
		}
	}
	return request{}
}

type fakeAccounts struct {
	mu      sync.Mutex
	ensured map[string]string
	ended   []string
}

func (f *fakeAccounts) Ensure(_ context.Context, idp, id string) (users.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch id {
	case "taken":
		return users.User{}, users.ErrExists
	case "off":
		return users.User{ID: id, IDP: idp, Disabled: true}, nil
	case "bad id":
		return users.User{}, users.ErrInvalid
	case "down":
		return users.User{}, errors.New("users database unreachable")
	}
	if f.ensured == nil {
		f.ensured = map[string]string{}
	}
	f.ensured[id] = idp
	return users.User{ID: id, IDP: idp}, nil
}

func (f *fakeAccounts) EndCredential(_ context.Context, credential string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, credential)
}

func (f *fakeAccounts) endedCredentials() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ended...)
}
