// Package controlmcp serves the control MCP: lifecycle tools on the caller's own boxes over the Boxes service.
package controlmcp

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/mcpwire"
	"github.com/loomhand/cocoonbox/api/oauth"
)

const (
	// Path is the control MCP endpoint, next to the per-box MCP paths that the edge proxies.
	Path = "/cocoonbox.mcp/control"

	revisionLen = 12
)

// Grants checks the OAuth access tokens of agents and keeps the boxes each grant created.
type Grants interface {
	Authenticate(ctx context.Context, header string) (oauth.Grant, error)
	Boxes(ctx context.Context, grant string) ([]string, error)
	AddBox(ctx context.Context, grant, box string) error
	Challenge(invalid bool) string
}

// Server answers MCP Streamable HTTP on Path for the boxes of the bearer token's user, or of its grant.
type Server struct {
	boxes  controlv1.BoxesServer
	authn  *auth.Authenticator
	grants Grants

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New serves the control tools over boxes, authenticating with authn.
func New(boxes controlv1.BoxesServer, authn *auth.Authenticator) *Server {
	return &Server{boxes: boxes, authn: authn, locks: map[string]*sync.Mutex{}}
}

// EnableGrants admits OAuth access tokens next to user tokens; a token acts only on the boxes its grant created, within its caps.
func (s *Server) EnableGrants(grants Grants) {
	s.grants = grants
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	user, credential, grant, err := s.authenticate(r)
	if err != nil {
		s.refuse(w, r, err)
		return
	}
	ctx, stop := s.authn.Bind(auth.WithUser(r.Context(), user), user, credential)
	defer stop()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeFailure(w, http.StatusBadRequest, mcpwire.CodeParse, err.Error())
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		if _, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			writeFailure(w, http.StatusBadRequest, mcpwire.CodeInvalidRequest, "expected one JSON-RPC message")
			return
		}
		writeFailure(w, http.StatusBadRequest, mcpwire.CodeParse, "parse error")
		return
	}
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	caller := user
	if grant != nil {
		ctx = context.WithValue(ctx, grantKey{}, grant)
		caller += " grant=" + grant.ID
	}
	var (
		call   callParams
		result any
		rpcErr *mcpwire.Error
	)
	defer func() {
		log.WithFunc("controlmcp.Server.ServeHTTP").Infof(ctx, "%s %s user=%s code=%d in %s",
			req.Method, cmp.Or(call.Name, "-"), caller, mcpwire.CodeOf(rpcErr), time.Since(start).Round(time.Millisecond))
	}()
	switch {
	case req.Method == "initialize":
		w.Header().Set(mcpwire.SessionHeader, rand.Text())
		result = initializeResult(req.Params)
	case req.Method != "tools/call":
		result, rpcErr = handle(req.Method)
	case json.Unmarshal(req.Params, &call) != nil:
		rpcErr = &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "invalid tools/call params"}
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

// authenticate maps the bearer to its user and credential, and an access token also to its grant, whose id is its credential.
func (s *Server) authenticate(r *http.Request) (string, string, *oauth.Grant, error) {
	header := r.Header.Get("Authorization")
	if s.grants == nil || !oauth.IsAccessToken(header) {
		user, err := s.authn.Authenticate(r.Context(), header)
		token, _ := auth.Bearer(header)
		return user, auth.TokenCredential(token), nil, err
	}
	g, err := s.grants.Authenticate(r.Context(), header)
	if err != nil {
		return "", "", nil, err
	}
	return g.User, g.ID, &g, nil
}

// refuse answers a failed authentication: 401 with the OAuth challenge, or 503 when the grant store failed.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, err error) {
	if s.grants == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if oauth.IsAccessToken(r.Header.Get("Authorization")) && !errors.Is(err, oauth.ErrInvalidToken) {
		log.WithFunc("controlmcp.Server.refuse").Error(r.Context(), err, "check access token")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("WWW-Authenticate", s.grants.Challenge(r.Header.Get("Authorization") != ""))
	w.WriteHeader(http.StatusUnauthorized)
}

// lock serializes the calls of one grant that may start a box, so two cannot both pass its caps.
func (s *Server) lock(grant string) func() {
	s.mu.Lock()
	l := s.locks[grant]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[grant] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

type grantKey struct{}

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
	Error   *mcpwire.Error  `json:"error,omitempty"`
}

func newResponse(id json.RawMessage, result any, rpcErr *mcpwire.Error) response {
	if rpcErr != nil {
		return response{JSONRPC: mcpwire.Version, ID: id, Error: rpcErr}
	}
	return response{JSONRPC: mcpwire.Version, ID: id, Result: result}
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
	w.Header().Set("Content-Type", mcpwire.EventStream)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	e := &eventStream{w: w, rc: http.NewResponseController(w), token: token, stop: make(chan struct{})}
	e.done.Go(e.keepalive)
	return e
}

func (e *eventStream) progress(message string) {
	e.sent++
	e.send(notification{
		JSONRPC: mcpwire.Version,
		Method:  "notifications/progress",
		Params:  progressParams{ProgressToken: e.token, Progress: e.sent, Message: message},
	})
}

func (e *eventStream) send(msg any) {
	data, err := marshal(msg)
	if err != nil {
		return
	}
	e.write(mcpwire.Event(bytes.TrimSpace(data)))
}

func (e *eventStream) close() {
	close(e.stop)
	e.done.Wait()
}

// keepalive writes an SSE comment while a long operation reports nothing, so idle proxies keep the stream.
func (e *eventStream) keepalive() {
	ticker := time.NewTicker(mcpwire.Keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.write([]byte(mcpwire.KeepaliveComment))
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
	version := mcpwire.Negotiate(req.ProtocolVersion)
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
func handle(method string) (any, *mcpwire.Error) {
	switch method {
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": toolSpecs}, nil
	default:
		return nil, &mcpwire.Error{Code: mcpwire.CodeMethodNotFound, Message: "method not found: " + method}
	}
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, mcpwire.MaxBody))
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return nil, fmt.Errorf("body exceeds %d bytes", mcpwire.MaxBody)
	}
	return buf.Bytes(), err
}

func writeFailure(w http.ResponseWriter, httpStatus, code int, message string) {
	writeJSON(w, httpStatus, response{JSONRPC: mcpwire.Version, Error: &mcpwire.Error{Code: code, Message: message}})
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
