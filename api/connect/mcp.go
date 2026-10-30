package connect

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/mcpwire"
)

const (
	// MCPPath is the mode B door, next to the control MCP on the edge's MCP listener.
	MCPPath = "/cocoonbox.mcp/connect"

	sessionTTL    = 24 * time.Hour
	maxClientName = 64
	answersQueued = 8
)

var listChanged = mcpwire.Event([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))

// Authenticator maps an Authorization header value to its user and binds a call to the user and credential, so it ends when the user is disabled or deleted or the credential ends.
type Authenticator interface {
	Authenticate(ctx context.Context, entry auth.Entry, header string) (auth.Identity, error)
	Bind(ctx context.Context, user, credential string) (context.Context, context.CancelFunc)
}

// authFunc maps an Authorization header value to who it admits.
type authFunc func(ctx context.Context, header string) (admitted, error)

// admitted is a door call's user, the calling machine on the box door, and the credential the call is bound to.
type admitted struct {
	user       string
	machine    string
	credential string
}

type session struct {
	client string
	at     time.Time
}

type watcher struct {
	changed  chan struct{}
	answered chan []byte
}

type response struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id,omitzero"`
	Result  any            `json:"result,omitzero"`
	Error   *mcpwire.Error `json:"error,omitzero"`
}

// Door serves the connector tools of the bearer's user over MCP Streamable HTTP for one door.
type Door struct {
	svc   *Service
	authn authFunc
	bind  func(ctx context.Context, user, credential string) (context.Context, context.CancelFunc)
	door  string

	mu      sync.Mutex
	clients map[string]session
}

func (d *Door) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	in, err := d.authn(r.Context(), r.Header.Get(headerAuthorization))
	if err != nil {
		w.WriteHeader(refusal(err))
		return
	}
	user, machine := in.user, in.machine
	ctx, stop := d.bind(r.Context(), user, in.credential)
	defer stop()
	r = r.WithContext(ctx)
	switch r.Method {
	case http.MethodPost:
	case http.MethodGet:
		if !strings.Contains(r.Header.Get("Accept"), mcpwire.EventStream) {
			http.Error(w, "accept text/event-stream", http.StatusNotAcceptable)
			return
		}
		d.stream(w, r, user)
		return
	case http.MethodDelete:
		d.mu.Lock()
		delete(d.clients, r.Header.Get(mcpwire.SessionHeader))
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpwire.MaxBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, response{JSONRPC: mcpwire.Version, Error: &mcpwire.Error{Code: mcpwire.CodeParse, Message: fmt.Sprintf("read body: %v", err)}})
		return
	}
	var req struct {
		ID     jsontext.Value `json:"id"`
		Method string         `json:"method"`
		Params jsontext.Value `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{JSONRPC: mcpwire.Version, Error: &mcpwire.Error{Code: mcpwire.CodeInvalidRequest, Message: "expected one JSON-RPC message"}})
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	c := caller{user: user, door: d.door, name: d.door + ":" + cmp.Or(machine, d.client(r.Header.Get(mcpwire.SessionHeader)))}
	var (
		call struct {
			Name      string         `json:"name"`
			Arguments jsontext.Value `json:"arguments"`
			Meta      struct {
				ApprovalID string `json:"cocoonbox/approval_id"`
			} `json:"_meta"`
		}
		result any
		rpcErr *mcpwire.Error
	)
	defer func() {
		log.WithFunc("connect.Door.ServeHTTP").Infof(r.Context(), "%s %s %s user=%s code=%d in %s",
			d.door, req.Method, cmp.Or(call.Name, "-"), user, mcpwire.CodeOf(rpcErr), time.Since(start).Round(time.Millisecond))
	}()
	switch req.Method {
	case methodInitialize:
		result = d.initialize(w, req.Params)
	case "ping":
		result = struct{}{}
	case "tools/list":
		tools, err := d.svc.doorTools(r.Context(), c)
		if err != nil {
			rpcErr = internalError(r, err)
			break
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		if err := json.Unmarshal(req.Params, &call); err != nil || call.Name == "" {
			rpcErr = &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "invalid tools/call params"}
			break
		}
		out, err := d.svc.doorCall(r.Context(), c, call.Name, call.Arguments, call.Meta.ApprovalID)
		if errors.As(err, &rpcErr) {
			break
		}
		if err != nil {
			rpcErr = internalError(r, err)
			break
		}
		result = out
	default:
		rpcErr = &mcpwire.Error{Code: mcpwire.CodeMethodNotFound, Message: "method not found: " + req.Method}
	}
	resp := response{JSONRPC: mcpwire.Version, ID: req.ID, Result: result}
	if rpcErr != nil {
		resp.Result, resp.Error = nil, rpcErr
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d *Door) initialize(w http.ResponseWriter, params jsontext.Value) map[string]any {
	var req struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
	}
	_ = json.Unmarshal(params, &req)
	id := rand.Text()
	now := time.Now()
	d.mu.Lock()
	maps.DeleteFunc(d.clients, func(_ string, v session) bool { return now.Sub(v.at) > sessionTTL })
	d.clients[id] = session{client: shorten(cmp.Or(req.ClientInfo.Name, "unknown"), maxClientName), at: now}
	d.mu.Unlock()
	w.Header().Set(mcpwire.SessionHeader, id)
	return map[string]any{
		"protocolVersion": mcpwire.Negotiate(req.ProtocolVersion),
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
		"serverInfo":      implementation{Name: clientName, Version: "1"},
	}
}

func (d *Door) client(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.clients[id]; ok {
		return s.client
	}
	return "unknown"
}

func (d *Door) stream(w http.ResponseWriter, r *http.Request, user string) {
	watch, stop := d.svc.watchers.add(user, d.door == DoorBox)
	defer stop()
	var replay []*approval
	if d.door == DoorBox {
		var err error
		if replay, err = d.svc.store.answered(r.Context(), user); err != nil {
			log.WithFunc("connect.Door.stream").Errorf(r.Context(), err, "list the answered approvals of %s", user)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", mcpwire.EventStream)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for _, a := range replay {
		if _, err := w.Write(approvalAnswered(a.id, a.decision)); err != nil {
			return
		}
	}
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	tick := time.Tick(mcpwire.Keepalive)
	for {
		var msg []byte
		select {
		case <-r.Context().Done():
			return
		case <-watch.changed:
			msg = listChanged
		case msg = <-watch.answered:
		case <-tick:
			msg = []byte(mcpwire.KeepaliveComment)
		}
		if _, err := w.Write(msg); err != nil || rc.Flush() != nil {
			return
		}
	}
}

// refusal is the status of a failed authentication: 503 when the identity provider could not answer, else 401.
func refusal(err error) int {
	switch status.Code(err) {
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.PermissionDenied:
		return http.StatusForbidden
	}
	return http.StatusUnauthorized
}

// NewDoor serves the tools for door, authenticating with authn.
func (s *Service) NewDoor(authn Authenticator, door string) *Door {
	return &Door{svc: s, door: door, clients: map[string]session{}, bind: authn.Bind, authn: func(ctx context.Context, header string) (admitted, error) {
		id, err := authn.Authenticate(ctx, auth.EntryMCP, header)
		return admitted{user: id.User, credential: id.Credential}, err
	}}
}

// NewBoxDoor serves the box door, authenticating each call by the door key of its machine and binding it to the machine's owner with authn.
func (s *Service) NewBoxDoor(authn Authenticator) *Door {
	return &Door{svc: s, door: DoorBox, clients: map[string]session{}, bind: authn.Bind, authn: func(ctx context.Context, header string) (admitted, error) {
		user, machine, err := s.BoxCaller(ctx, header)
		return admitted{user: user, machine: machine}, err
	}}
}

type watchers struct {
	mu     sync.Mutex
	byUser map[string]map[*watcher]struct{}
}

func newWatchers() *watchers {
	return &watchers{byUser: map[string]map[*watcher]struct{}{}}
}

func (w *watchers) add(user string, box bool) (*watcher, func()) {
	watch := &watcher{changed: make(chan struct{}, 1)}
	if box {
		watch.answered = make(chan []byte, answersQueued)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.byUser[user] == nil {
		w.byUser[user] = map[*watcher]struct{}{}
	}
	w.byUser[user][watch] = struct{}{}
	return watch, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.byUser[user], watch)
		if len(w.byUser[user]) == 0 {
			delete(w.byUser, user)
		}
	}
}

func (w *watchers) notify(user string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for watch := range w.byUser[user] {
		select {
		case watch.changed <- struct{}{}:
		default:
		}
	}
}

// notifyAll wakes every watcher, for the changes another api signaled while none listened.
func (w *watchers) notifyAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, byWatch := range w.byUser {
		for watch := range byWatch {
			select {
			case watch.changed <- struct{}{}:
			default:
			}
		}
	}
}

func (w *watchers) boxUsers() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var users []string
	for user, byWatch := range w.byUser {
		for watch := range byWatch {
			if watch.answered != nil {
				users = append(users, user)
				break
			}
		}
	}
	return users
}

func (w *watchers) answer(user string, msg []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for watch := range w.byUser[user] {
		select {
		case watch.answered <- msg:
		default:
		}
	}
}

func approvalAnswered(id, decision string) []byte {
	var msg struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			ApprovalID string `json:"approval_id"`
			Decision   string `json:"decision"`
		} `json:"params"`
	}
	msg.JSONRPC, msg.Method = mcpwire.Version, "notifications/cocoonbox/approval_answered"
	msg.Params.ApprovalID, msg.Params.Decision = id, decision
	data, _ := json.Marshal(msg)
	return mcpwire.Event(data)
}

func internalError(r *http.Request, err error) *mcpwire.Error {
	log.WithFunc("connect.internalError").Errorf(r.Context(), err, "%s %s", r.Method, r.URL.Path)
	return &mcpwire.Error{Code: mcpwire.CodeInternal, Message: "internal error"}
}

func writeJSON(w http.ResponseWriter, httpStatus int, body any) {
	var buf bytes.Buffer
	if err := json.MarshalWrite(&buf, body, json.Deterministic(true)); err != nil {
		httpStatus = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"encode response"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_, _ = w.Write(buf.Bytes())
}
