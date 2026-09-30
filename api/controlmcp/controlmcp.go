// Package controlmcp serves the control MCP: lifecycle tools on the caller's own box over the Boxes service.
package controlmcp

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

const (
	// Path is the control MCP endpoint, next to the per-box MCP paths that the edge proxies.
	Path = "/cocoonbox.mcp/control"

	jsonRPCVersion  = "2.0"
	fallbackVersion = "2025-11-25"
	sessionHeader   = "Mcp-Session-Id"
	revisionLen     = 12
	maxBody         = 1 << 20
	keepalive       = 25 * time.Second

	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

var protocolVersions = []string{"2025-11-25", "2025-06-18"}

// Server answers MCP Streamable HTTP on Path for the box of the bearer token's user.
type Server struct {
	boxes controlv1.BoxesServer
	authn *auth.Authenticator
}

// New serves the control tools over boxes, authenticating with authn.
func New(boxes controlv1.BoxesServer, authn *auth.Authenticator) *Server {
	return &Server{boxes: boxes, authn: authn}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	user, err := s.authn.Authenticate(r.Header.Get("Authorization"))
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeFailure(w, http.StatusBadRequest, codeParse, err.Error())
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		if typeErr := (*json.UnmarshalTypeError)(nil); errors.As(err, &typeErr) {
			writeFailure(w, http.StatusBadRequest, codeInvalidRequest, "expected one JSON-RPC message")
			return
		}
		writeFailure(w, http.StatusBadRequest, codeParse, "parse error")
		return
	}
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	ctx := auth.WithUser(r.Context(), user)
	var (
		call   callParams
		result any
		rpcErr *rpcError
	)
	defer func() {
		log.WithFunc("controlmcp.Server.ServeHTTP").Infof(ctx, "%s %s user=%s code=%d in %s",
			req.Method, cmp.Or(call.Name, "-"), user, rpcErr.code(), time.Since(start).Round(time.Millisecond))
	}()
	switch {
	case req.Method == "initialize":
		w.Header().Set(sessionHeader, rand.Text())
		result = initializeResult(req.Params)
	case req.Method != "tools/call":
		result, rpcErr = handle(req.Method)
	case json.Unmarshal(req.Params, &call) != nil:
		rpcErr = &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params"}
	case call.Meta.ProgressToken != nil:
		stream := newEventStream(w, call.Meta.ProgressToken)
		defer stream.close()
		result, rpcErr = s.call(ctx, &call, stream.progress)
		stream.send(newResponse(req.ID, result, rpcErr))
		return
	default:
		result, rpcErr = s.call(ctx, &call, func(string) {})
	}
	writeJSON(w, http.StatusOK, newResponse(req.ID, result, rpcErr))
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      struct {
		ProgressToken json.RawMessage `json:"progressToken"`
	} `json:"_meta"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func newResponse(id json.RawMessage, result any, rpcErr *rpcError) response {
	if rpcErr != nil {
		return response{JSONRPC: jsonRPCVersion, ID: id, Error: rpcErr}
	}
	return response{JSONRPC: jsonRPCVersion, ID: id, Result: result}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) code() int {
	if e == nil {
		return 0
	}
	return e.Code
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type progressParams struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      int             `json:"progress"`
	Message       string          `json:"message"`
}

// eventStream answers one tools/call as server-sent events: progress notifications, then the response.
type eventStream struct {
	mu    sync.Mutex
	w     http.ResponseWriter
	rc    *http.ResponseController
	token json.RawMessage
	sent  int
	stop  chan struct{}
	done  sync.WaitGroup
}

func newEventStream(w http.ResponseWriter, token json.RawMessage) *eventStream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	e := &eventStream{w: w, rc: http.NewResponseController(w), token: token, stop: make(chan struct{})}
	e.done.Go(e.keepalive)
	return e
}

func (e *eventStream) progress(message string) {
	e.sent++
	e.send(notification{
		JSONRPC: jsonRPCVersion,
		Method:  "notifications/progress",
		Params:  progressParams{ProgressToken: e.token, Progress: e.sent, Message: message},
	})
}

func (e *eventStream) send(msg any) {
	data, err := marshal(msg)
	if err != nil {
		return
	}
	e.write(fmt.Appendf(nil, "event: message\ndata: %s\n\n", bytes.TrimSpace(data)))
}

func (e *eventStream) close() {
	close(e.stop)
	e.done.Wait()
}

// keepalive writes an SSE comment while a long operation reports nothing, so idle proxies keep the stream.
func (e *eventStream) keepalive() {
	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.write([]byte(": keepalive\n\n"))
		}
	}
}

func (e *eventStream) write(b []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.w.Write(b); err == nil {
		_ = e.rc.Flush()
	}
}

func initializeResult(params json.RawMessage) map[string]any {
	var req struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &req)
	version := fallbackVersion
	if slices.Contains(protocolVersions, req.ProtocolVersion) {
		version = req.ProtocolVersion
	}
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "cocoonbox-control", "version": buildVersion(settings)},
	}
}

func buildVersion(settings []debug.BuildSetting) string {
	var revision string
	var modified bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return "(devel)"
	}
	revision = revision[:min(len(revision), revisionLen)]
	if modified {
		revision += "-dirty"
	}
	return revision
}

// handle answers the methods other than initialize and tools/call.
func handle(method string) (any, *rpcError) {
	switch method {
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": toolSpecs}, nil
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
	}
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, maxBody))
	if maxErr := (*http.MaxBytesError)(nil); errors.As(err, &maxErr) {
		return nil, fmt.Errorf("body exceeds %d bytes", maxBody)
	}
	return buf.Bytes(), err
}

func writeFailure(w http.ResponseWriter, httpStatus, code int, message string) {
	writeJSON(w, httpStatus, response{JSONRPC: jsonRPCVersion, Error: &rpcError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, httpStatus int, body any) {
	data, err := marshal(body)
	if err != nil {
		httpStatus = http.StatusInternalServerError
		data = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"encode response"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_, _ = w.Write(data)
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(v)
	return buf.Bytes(), err
}
