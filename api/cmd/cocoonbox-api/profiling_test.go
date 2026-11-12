package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"testing"
	"time"
)

func TestTheCPUProfileServesOneFixedCaptureAtATimeUntilTheSignal(t *testing.T) {
	signals, signal := context.WithCancel(t.Context())
	t.Cleanup(signal)
	client := &http.Client{Timeout: cpuProfileWriteTimeout}
	t.Cleanup(client.CloseIdleConnections)

	t.Run("off opens nothing and an occupied port fails the start", func(t *testing.T) {
		if err := serveCPUProfile(signals, false); err != nil {
			t.Fatalf("off: %v", err)
		}
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer occupied.Close()
		if _, err := listenCPUProfile(signals, occupied.Addr().String()); err == nil {
			t.Fatal("served despite the occupied port")
		}
	})
	served, err := listenCPUProfile(signals, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := served.String()

	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/debug/pprof/", http.StatusNotFound},
		{http.MethodGet, "/debug/pprof/heap", http.StatusNotFound},
		{http.MethodGet, "/debug/pprof/goroutine", http.StatusNotFound},
		{http.MethodGet, "/debug/pprof/trace", http.StatusNotFound},
		{http.MethodGet, "/debug/pprof/cmdline", http.StatusNotFound},
		{http.MethodHead, "/debug/pprof/profile", http.StatusMethodNotAllowed},
		{http.MethodPost, "/debug/pprof/profile", http.StatusMethodNotAllowed},
		{http.MethodGet, "/debug/pprof/profile?seconds=3600", http.StatusBadRequest},
		{http.MethodGet, "/debug/pprof/profile?seconds=0", http.StatusBadRequest},
		{http.MethodGet, "/debug/pprof/profile?unknown=value", http.StatusBadRequest},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := profileRequest(t, client, addr, tc.method, tc.path)
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.status == http.StatusMethodNotAllowed && resp.Header.Get("Allow") != http.MethodGet {
				t.Errorf("Allow = %q, want GET", resp.Header.Get("Allow"))
			}
		})
	}

	t.Run("a disconnect ends its capture before the timer", func(t *testing.T) {
		ctx, disconnect := context.WithCancel(t.Context())
		defer disconnect()
		active := competingCaptures(t, ctx, client, addr)
		disconnect()
		if stopped := <-active; stopped.resp != nil {
			_ = stopped.resp.Body.Close()
			t.Fatal("the disconnected capture completed")
		}
	})

	t.Run("a capture runs ten seconds and go tool pprof reads it", func(t *testing.T) {
		start := time.Now()
		resp := admittedCapture(t, client, addr)
		if resp.Header.Get("Content-Type") != "application/octet-stream" || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("profile headers = %v", resp.Header)
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read the capture: %v", err)
		}
		if elapsed := time.Since(start); elapsed < cpuProfileDuration-time.Second || elapsed > cpuProfileWriteTimeout-time.Second {
			t.Errorf("capture took %s, want about %s", elapsed, cpuProfileDuration)
		}
		profile := filepath.Join(t.TempDir(), "cpu.pprof")
		if err := os.WriteFile(profile, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.CommandContext(t.Context(), "go", "tool", "pprof", "-top", profile).CombinedOutput(); err != nil {
			t.Fatalf("go tool pprof could not read the capture: %v\n%s", err, output)
		}
	})

	t.Run("the signal ends a capture, stops the profiler and closes the listener", func(t *testing.T) {
		active := competingCaptures(t, t.Context(), client, addr)
		signal()
		select {
		case stopped := <-active:
			if stopped.resp != nil {
				_ = stopped.resp.Body.Close()
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the signal left the capture open")
		}
		deadline := time.Now().Add(3 * time.Second)
		for pprof.StartCPUProfile(io.Discard) != nil {
			if time.Now().After(deadline) {
				t.Fatal("the signal left the CPU profiler running")
			}
			time.Sleep(10 * time.Millisecond)
		}
		pprof.StopCPUProfile()
		if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			_ = conn.Close()
			t.Fatal("the listener stayed open after the signal")
		}
	})
}

type captureResult struct {
	resp *http.Response
	err  error
}

func profileRequest(t *testing.T, client *http.Client, addr, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func competingCaptures(t *testing.T, ctx context.Context, client *http.Client, addr string) <-chan captureResult {
	t.Helper()
	results := make(chan captureResult, 2)
	for range 2 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/debug/pprof/profile", nil)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			resp, err := client.Do(req)
			results <- captureResult{resp, err}
		}()
	}
	refused := <-results
	if refused.err != nil {
		t.Fatalf("competing capture: %v", refused.err)
	}
	_ = refused.resp.Body.Close()
	if refused.resp.StatusCode != http.StatusConflict {
		t.Fatalf("overlapping capture = %d, want 409", refused.resp.StatusCode)
	}
	return results
}

func admittedCapture(t *testing.T, client *http.Client, addr string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := profileRequest(t, client, addr, http.MethodGet, "/debug/pprof/profile")
		if resp.StatusCode == http.StatusOK {
			return resp
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("capture status = %d; the profiler did not free up", resp.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
