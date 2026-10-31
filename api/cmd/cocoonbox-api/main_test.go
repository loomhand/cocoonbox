package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/config"
	"github.com/loomhand/cocoonbox/api/users"
)

func TestTheContextOutlivesTheSignalByTheDrainWhileHealthzFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signals, stop := context.WithCancel(t.Context())
		var draining atomic.Bool
		ctx, cancel := drainAfter(signals, 3*time.Second, &draining)
		defer cancel()
		health := func() int {
			rec := httptest.NewRecorder()
			healthz(&draining)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			return rec.Code
		}
		if code := health(); code != http.StatusOK {
			t.Fatalf("healthz before the signal = %d, want 200", code)
		}
		stop()
		synctest.Wait()
		if code := health(); code != http.StatusServiceUnavailable || ctx.Err() != nil {
			t.Fatalf("after the signal: healthz %d, context %v, want 503 and a live context", code, ctx.Err())
		}
		time.Sleep(3*time.Second - time.Millisecond)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatal("the context ended before the drain")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if ctx.Err() == nil {
			t.Error("the context outlived the drain")
		}
	})
}

func TestNoDrainEndsTheContextWithTheSignal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signals, stop := context.WithCancel(t.Context())
		var draining atomic.Bool
		ctx, cancel := drainAfter(signals, 0, &draining)
		defer cancel()
		stop()
		synctest.Wait()
		if ctx.Err() == nil {
			t.Error("without a drain the context outlived the signal")
		}
	})
}

func TestOnlyAnIdentityURLSendsOutsideTokensToItsWebhook(t *testing.T) {
	ep := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer caller-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"authenticated":true,"allowed":true,"idp":"example","user":"Fb7xQ2kLm9PzR4tVw1YcN8sJd3Hq","credential":"c1","ttlSeconds":60}`)
	}))
	defer ep.Close()
	transport := http.DefaultTransport.(*http.Transport)
	trusted := transport.TLSClientConfig
	transport.TLSClientConfig = ep.Client().Transport.(*http.Transport).TLSClientConfig
	t.Cleanup(func() { transport.TLSClientConfig = trusted })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "identity-token"), []byte("caller-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	accounts := &oneUser{}
	off, err := openIdentities(t.Context(), &config.Config{}, accounts)
	if err != nil {
		t.Fatalf("openIdentities without identity: %v", err)
	}
	if _, err := off.Resolve(t.Context(), "sapi_0123", auth.EntryMCP); !errors.Is(err, auth.ErrUnknownToken) {
		t.Errorf("an outside token without identity = %v, want ErrUnknownToken", err)
	}
	for name, identity := range map[string]config.Identity{
		"file": {URL: ep.URL, TokenFile: filepath.Join(dir, "identity-token"), Timeout: config.Duration(time.Second)},
		"none": {URL: ep.URL, Timeout: config.Duration(time.Second)},
	} {
		on, err := openIdentities(t.Context(), &config.Config{Identity: identity}, accounts)
		if err != nil {
			t.Fatalf("%s: openIdentities: %v", name, err)
		}
		id, err := on.Resolve(t.Context(), "sapi_0123", auth.EntryMCP)
		if admitted := err == nil && id.User == "Fb7xQ2kLm9PzR4tVw1YcN8sJd3Hq" && accounts.idp == "example"; admitted != (name != "none") {
			t.Errorf("%s: an outside token = %+v, %v, ensured from %q", name, id, err, accounts.idp)
		}
		if id, err := on.Resolve(t.Context(), "cb_0123", auth.EntryMCP); err != nil || id.User != "u-local" {
			t.Errorf("%s: a cocoonbox token = %+v, %v, want the local user", name, id, err)
		}
	}
	if _, err := openIdentities(t.Context(), &config.Config{Identity: config.Identity{URL: "http://id.example"}}, accounts); err == nil {
		t.Error("openIdentities over http opened")
	}
}

type oneUser struct{ idp string }

func (*oneUser) ByTokenSHA256(_ context.Context, sum string) (string, bool) {
	return "u-local", sum == auth.TokenCredential("cb_0123")
}

func (*oneUser) Lifetime(string, string) (context.Context, func()) {
	return context.Background(), func() {}
}

func (o *oneUser) Ensure(_ context.Context, idp, id string) (users.User, error) {
	o.idp = idp
	return users.User{ID: id, IDP: idp}, nil
}
