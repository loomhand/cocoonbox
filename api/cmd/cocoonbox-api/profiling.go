package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
)

const (
	cpuProfileAddr         = "127.0.0.1:18203"
	cpuProfileDuration     = 10 * time.Second
	cpuProfileWriteTimeout = 15 * time.Second
)

// serveCPUProfile serves one fixed CPU capture at a time on loopback when enabled, until signals ends; it never waits for the drain.
func serveCPUProfile(signals context.Context, enabled bool) error {
	if !enabled {
		return nil
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(signals, "tcp", cpuProfileAddr)
	if err != nil {
		return err
	}
	var capture sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/profile", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet:
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		case r.URL.RawQuery != "":
			http.Error(w, "query parameters are not supported", http.StatusBadRequest)
			return
		case !capture.TryLock():
			http.Error(w, "a CPU capture is already running", http.StatusConflict)
			return
		}
		defer capture.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		if err := pprof.StartCPUProfile(w); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		defer pprof.StopCPUProfile()
		timer := time.NewTimer(cpuProfileDuration)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
		}
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      cpuProfileWriteTimeout,
		BaseContext:       func(net.Listener) context.Context { return signals },
	}
	context.AfterFunc(signals, func() { _ = server.Close() })
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithFunc("main.serveCPUProfile").Error(signals, err, "serve the CPU profile")
		}
	}()
	return nil
}
