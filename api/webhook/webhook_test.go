package webhook

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if got := ep.last(); got.bearer != "Bearer caller-token" || got.Token != "sapi_alice" || got.Entry != "mcp" {
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

func TestAnEndpointThatRefusesTheApisBearerIsAnOutage(t *testing.T) {
	ep := newEndpoint(t)
	ep.admit("sapi_alice", "alice", "cred-alice", 60)
	ep.fail(http.StatusUnauthorized)
	hook := newHook(t, ep, &fakeAccounts{})
	if _, err := hook.Resolve(t.Context(), "sapi_alice", auth.EntryMCP); err == nil || errors.Is(err, auth.ErrUnknownToken) {
		t.Errorf("Resolve with the api's bearer refused = %v, want an outage", err)
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
	hook, err := New(Options{URL: ep.URL, TokenFile: tokenFile(t), Timeout: time.Second}, &fakeAccounts{})
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
	hook, err := New(Options{URL: ep.URL, TokenFile: tokenFile(t), Timeout: time.Second}, accounts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hook.http.Transport = ep.Client().Transport
	return hook
}

func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("caller-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type request struct {
	bearer string
	Token  string `json:"token"`
	Entry  string `json:"entry"`
}

type endpoint struct {
	*httptest.Server
	mu       sync.Mutex
	reviews  map[string]review
	raw      map[string]string
	requests []request
	failing  int
}

func newEndpoint(t *testing.T) *endpoint {
	ep := &endpoint{reviews: map[string]review{}, raw: map[string]string{}}
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
	if raw, ok := e.raw[req.Token]; ok {
		_, _ = w.Write([]byte(raw))
		return
	}
	answer := review{TTLSeconds: 5}
	if rv, ok := e.reviews[req.Token]; ok {
		answer = rv
	}
	_ = json.MarshalWrite(w, answer)
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

type fakeAccounts struct {
	mu      sync.Mutex
	ensured map[string]string
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
