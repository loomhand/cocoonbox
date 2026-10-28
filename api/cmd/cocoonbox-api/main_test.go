package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
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
