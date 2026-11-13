package controlmcp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/boxes"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/mcpwire"
	"github.com/loomhand/cocoonbox/api/users"
)

const (
	// wakeBound sits under a client's 60 s tool timeout with room for the call itself; a wake takes 5 to 17 s.
	wakeBound     = 25 * time.Second
	readyInterval = 500 * time.Millisecond
	sessionIdle   = 24 * time.Hour
	maxSessions   = 16
	closeBound    = 10 * time.Second
	// rereadEvery picks up an agent session another api process opened instead, and keeps the record from the idle purge.
	rereadEvery = time.Hour
	// maxInstructions is the Mac bridge's cap on a server's instructions, within every agent CLI's limit.
	maxInstructions = 2048
	maxAnswer       = 32 << 20
	acceptBoth      = "application/json, " + mcpwire.EventStream
	unifiedHead     = "These tools run on the user's cocoonbox, a cloud computer, not on this machine. box_* tools manage the user's boxes; the other tools act on the box their box argument names, else the default box, and wake it when it sleeps."
	boxArgument     = `{"type":"string","description":"The box's name or id, as box_list lists them; without it, the default box."}`

	errStillStarting toolError = "The box is still starting; call the tool again in a few seconds."
)

var (
	errNoBoxYet     = status.Error(codes.Unavailable, "the user has no box yet")
	errSessionStore = status.Error(codes.Internal, "the api could not use its record of the box's agent session; try again")

	boxField = []byte(`"box"`)

	defaultInitialize = jsontext.Value(`{"protocolVersion":"` + mcpwire.Latest + `","capabilities":{},"clientInfo":{"name":"` + unifiedName + `"}}`)
)

// agentSessions are the agent sessions held open, one per box: a user's client session's own, or the ones every client session of a grant shares.
type agentSessions struct {
	client string
	held   AgentSessionStore

	mu       sync.Mutex
	upstream map[string]string
	read     map[string]time.Time
	probe    map[string]bool
}

func newAgentSessions(client string, held AgentSessionStore) *agentSessions {
	return &agentSessions{client: client, held: held, upstream: map[string]string{}, read: map[string]time.Time{}, probe: map[string]bool{}}
}

func (a *agentSessions) drop(box, upstream string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.upstream[box] != upstream {
		return false
	}
	delete(a.upstream, box)
	a.probe[box] = true
	return true
}

func (a *agentSessions) retire(ctx context.Context, box, upstream string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.upstream[box] != upstream {
		return false
	}
	delete(a.upstream, box)
	if a.held != nil {
		log.WithFunc("controlmcp.agentSessions.retire").Error(ctx, a.held.DropAgentSession(ctx, a.client, box, upstream), "drop the shared agent session")
	}
	return true
}

// session is what the api keeps of one client session on RootPath: the client's initialize, its agent sessions and its default box.
type session struct {
	params jsontext.Value
	used   time.Time
	agents *agentSessions

	mu    sync.Mutex
	box   string
	image string
}

// reset drops box's agent session while it is still upstream, and then the default box when it is box, so the next call finds both again.
func (s *session) reset(ctx context.Context, box, upstream string) {
	if !s.agents.retire(ctx, box, upstream) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.box == box {
		s.box, s.image = "", ""
	}
}

func (s *session) reimage(box string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.box == box {
		s.image = ""
	}
}

func (s *Server) openSession(ctx context.Context, caller, id string, params jsontext.Value) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keepSession(ctx, caller, id, &session{params: params})
}

// session returns caller's client session id, made anew when this api does not know it, as after a restart.
func (s *Server) session(ctx context.Context, caller, id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[caller+"\n"+id]; sess != nil {
		sess.used = time.Now()
		return sess
	}
	return s.keepSession(ctx, caller, id, &session{})
}

// keepSession records sess, dropping sessions idle for sessionIdle and caller's least recently used past maxSessions.
func (s *Server) keepSession(ctx context.Context, caller, id string, sess *session) *session {
	now := time.Now()
	maps.DeleteFunc(s.sessions, func(_ string, old *session) bool { return now.Sub(old.used) > sessionIdle })
	var held []string
	for key := range s.sessions {
		if strings.HasPrefix(key, caller+"\n") {
			held = append(held, key)
		}
	}
	if len(held) >= maxSessions {
		delete(s.sessions, slices.MinFunc(held, func(a, b string) int { return s.sessions[a].used.Compare(s.sessions[b].used) }))
	}
	sess.used, sess.agents = now, s.agentsOf(ctx, caller, id)
	s.sessions[caller+"\n"+id] = sess
	return sess
}

// agentsOf returns the agent sessions of caller's new client session id: its own for a user token, and for a grant the grant's, closed when the grant ends; s.mu is held.
func (s *Server) agentsOf(ctx context.Context, caller, id string) *agentSessions {
	g := grantOf(ctx)
	if g == nil {
		return newAgentSessions(sessionClient(caller, id), s.held)
	}
	if a := s.shared[g.ID]; a != nil {
		return a
	}
	a := newAgentSessions(grantClient(g.ID), s.held)
	s.shared[g.ID] = a
	life, stop := s.authn.Bind(context.Background(), g.User, g.ID)
	context.AfterFunc(life, func() {
		stop()
		s.closeShared(g.ID, g.User, a)
	})
	return a
}

// closeShared forgets grant id's agent sessions a and closes them in their boxes.
func (s *Server) closeShared(id, user string, a *agentSessions) {
	s.mu.Lock()
	if s.shared[id] == a {
		delete(s.shared, id)
	}
	s.mu.Unlock()
	a.mu.Lock()
	upstreams := maps.Clone(a.upstream)
	clear(a.upstream)
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), closeBound)
	defer cancel()
	s.closeAgents(ctx, user, s.dropHeld(ctx, a.client, upstreams))
}

// closeAgents ends user's agent sessions upstreams, keyed by box.
func (s *Server) closeAgents(ctx context.Context, user string, upstreams map[string]string) {
	for box, upstream := range upstreams {
		_ = s.agents.Exchange(agentRequest(ctx, http.MethodDelete, nil, upstream, acceptBoth), user, box, func(*http.Response) error { return nil })
	}
}

// endSession forgets caller's client session id and closes its agent sessions, unless its grant shares them.
func (s *Server) endSession(ctx context.Context, user, caller, id string) {
	s.mu.Lock()
	sess := s.sessions[caller+"\n"+id]
	delete(s.sessions, caller+"\n"+id)
	s.mu.Unlock()
	if grantOf(ctx) != nil {
		return
	}
	upstreams := map[string]string{}
	if sess != nil {
		sess.agents.mu.Lock()
		upstreams = maps.Clone(sess.agents.upstream)
		sess.agents.mu.Unlock()
	}
	s.closeAgents(ctx, user, s.dropHeld(ctx, sessionClient(caller, id), upstreams))
}

// dropHeld adds to upstreams the shared agent sessions of client, which it forgets, so another process's are closed too.
func (s *Server) dropHeld(ctx context.Context, client string, upstreams map[string]string) map[string]string {
	if s.held == nil {
		return upstreams
	}
	held, err := s.held.DropAgentSessions(ctx, client)
	if err != nil {
		log.WithFunc("controlmcp.Server.dropHeld").Error(ctx, err, "drop the shared agent sessions")
		return upstreams
	}
	maps.Copy(upstreams, held)
	return upstreams
}

// initializeUnified answers an initialize on RootPath, opening the agent session of the default box when it runs; the instructions add the box's own when they fit.
func (s *Server) initializeUnified(ctx context.Context, user string, sess *session) map[string]any {
	logger := log.WithFunc("controlmcp.Server.initializeUnified")
	result := initializeResult(sess.params, unifiedName)
	result["instructions"] = unifiedHead
	if s.reachesAccounts(ctx) {
		result["capabilities"] = map[string]any{toolsField: map[string]any{"listChanged": true}}
	}
	box, image, err := s.defaultBox(ctx)
	if err != nil {
		return result
	}
	if box.GetState() == controlv1.BoxState_BOX_STATE_RUNNING {
		sess.mu.Lock()
		sess.box, sess.image = box.GetId(), image
		sess.mu.Unlock()
		if _, _, err = s.connect(ctx, user, sess, ""); err != nil {
			logger.Warnf(ctx, "open the agent session of box %s: %v", box.GetId(), err)
		}
	}
	l, err := s.lists.ToolList(ctx, image, boxes.Family(image))
	logger.Error(ctx, err, "read the tool list of "+image)
	if own := l.Instructions; own != "" && len(unifiedHead)+2+len(own) <= maxInstructions {
		result["instructions"] = unifiedHead + "\n\n" + own
	}
	return result
}

// listTools lists the control tools, then the default box's tools, sleeping or not, then the tools of the accounts a grant reaches that no other tool's name shadows.
func (s *Server) listTools(ctx context.Context) (any, *mcpwire.Error) {
	boxTools, rpcErr := s.boxTools(ctx)
	if rpcErr != nil {
		return nil, rpcErr
	}
	tools := make([]any, 0, len(toolSpecs)+len(boxTools))
	for _, t := range toolSpecs {
		tools = append(tools, t)
	}
	for _, t := range boxTools {
		tools = append(tools, withBoxArgument(t))
	}
	if s.reachesAccounts(ctx) {
		g := grantOf(ctx)
		accountTools, err := s.connectors.GrantedTools(ctx, g.User, true)
		if err != nil {
			log.WithFunc("controlmcp.Server.listTools").Error(ctx, err, "list the connector tools of grant "+g.ID)
		}
		boxNames := toolNames(boxTools)
		for _, t := range accountTools {
			var name string
			_ = json.Unmarshal(t["name"], &name)
			if !isControlTool(name) && !slices.Contains(boxNames, name) {
				tools = append(tools, t)
			}
		}
	}
	return map[string]any{toolsField: tools}, nil
}

// boxTools reads the default box's tools as the agent of its image serves them, sleeping or not.
func (s *Server) boxTools(ctx context.Context) ([]jsontext.Value, *mcpwire.Error) {
	_, image, err := s.defaultBox(ctx)
	if err != nil {
		return nil, &mcpwire.Error{Code: mcpwire.CodeInternal, Message: status.Convert(err).Message()}
	}
	var tools []jsontext.Value
	l, err := s.lists.ToolList(ctx, image, boxes.Family(image))
	if err == nil && len(l.Tools) != 0 {
		err = json.Unmarshal(l.Tools, &tools)
	}
	if err != nil {
		log.WithFunc("controlmcp.Server.boxTools").Error(ctx, err, "read the tool list of "+image)
		return nil, &mcpwire.Error{Code: mcpwire.CodeInternal, Message: "reading the box tools failed"}
	}
	return tools, nil
}

// reachesAccounts reports whether the caller's grant may use the user's connected accounts; a user token and a server without Connect reach none.
func (s *Server) reachesAccounts(ctx context.Context) bool {
	g := grantOf(ctx)
	return g != nil && s.connectors != nil && g.AllAccounts
}

// connectorTool reports whether name is a tool of an account the caller's grant reaches that no control or default box tool shadows; a failed lookup returns an error, never a box route.
func (s *Server) connectorTool(ctx context.Context, name string) (bool, *mcpwire.Error) {
	if !s.reachesAccounts(ctx) || isControlTool(name) {
		return false, nil
	}
	reaches, err := s.connectors.Reaches(ctx, grantOf(ctx).User, true, name)
	if err != nil {
		log.WithFunc("controlmcp.Server.connectorTool").Error(ctx, err, "read the connector of "+name)
		return false, &mcpwire.Error{Code: mcpwire.CodeInternal, Message: "reading the connector failed"}
	}
	if !reaches {
		return false, nil
	}
	boxTools, rpcErr := s.boxTools(ctx)
	return rpcErr == nil && !slices.Contains(toolNames(boxTools), name), rpcErr
}

// callConnector runs a tool of an account the caller's grant reaches through Connect; it wakes no box.
func (s *Server) callConnector(ctx context.Context, call *callParams) (any, *mcpwire.Error) {
	g := grantOf(ctx)
	out, err := s.connectors.CallGranted(ctx, g.User, g.ClientName, s.reachesAccounts(ctx), call.Name, call.Arguments)
	if rpcErr, ok := errors.AsType[*mcpwire.Error](err); ok {
		return nil, rpcErr
	}
	if err != nil {
		log.WithFunc("controlmcp.Server.callConnector").Error(ctx, err, "call "+call.Name)
		return nil, &mcpwire.Error{Code: mcpwire.CodeInternal, Message: "internal error"}
	}
	return out, nil
}

// callBox runs a box tool on the box its box argument names, else the session's default box; a box that is not running is woken first, within wakeBound.
func (s *Server) callBox(ctx context.Context, w http.ResponseWriter, user string, sess *session, id jsontext.Value, body []byte, accept string, call *callParams) {
	ref, body, err := takeBox(body, call)
	if err != nil {
		writeJSON(w, http.StatusOK, newResponse(id, nil, &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "invalid arguments: " + err.Error()}))
		return
	}
	box, stale, err := s.forward(ctx, user, sess, ref, body, accept, func(resp *http.Response) error { return relay(w, resp, nil) })
	if status.Code(err) != codes.Unavailable {
		if err != nil {
			writeJSON(w, http.StatusOK, newResponse(id, failure(err), nil))
		}
		return
	}
	var stream *eventStream
	progress := func(string) {}
	if call.Meta.ProgressToken != nil {
		stream = newEventStream(w, call.Meta.ProgressToken)
		progress = stream.progress
	}
	wakeCtx, cancel := context.WithTimeout(ctx, wakeBound)
	defer cancel()
	if err = s.wake(wakeCtx, sess, cmp.Or(box, ref), stale, progress); err == nil {
		err = s.ready(wakeCtx, user, sess, ref)
	}
	if stream != nil {
		stream.close()
	}
	switch {
	case err == nil:
		_, _, err = s.forward(ctx, user, sess, ref, body, accept, func(resp *http.Response) error { return relay(w, resp, stream) })
	case wakeCtx.Err() != nil && ctx.Err() == nil:
		err = errStillStarting
	}
	if err == nil {
		return
	}
	reply := newResponse(id, failure(err), nil)
	if stream != nil {
		stream.send(reply)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

// wake starts or wakes box, the user's first box made anew when it is empty, waits for it within ctx, and drops its agent session while it is still stale.
func (s *Server) wake(ctx context.Context, sess *session, box, stale string, progress progressFunc) error {
	res, err := s.ensure(ctx, box, true, progress)
	if err == nil {
		sess.agents.drop(res.BoxID, stale)
	}
	return err
}

// ready waits until the agent of the box ref names, else of the session's default box, answers an initialize, or ctx ends.
func (s *Server) ready(ctx context.Context, user string, sess *session, ref string) error {
	for {
		_, _, err := s.connect(ctx, user, sess, ref)
		if st, ok := status.FromError(err); !ok || (st.Code() != codes.Unavailable && st.Code() != codes.Unknown) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyInterval):
		}
	}
}

// forward sends body to the agent session of ref's box, else the default box, once more on a new session when the old one went, and returns the box and session it last tried.
func (s *Server) forward(ctx context.Context, user string, sess *session, ref string, body []byte, accept string, use func(*http.Response) error) (string, string, error) {
	for retried := false; ; retried = true {
		box, upstream, err := s.connect(ctx, user, sess, ref)
		forgot := false
		if err == nil {
			err = s.agents.Exchange(agentRequest(ctx, http.MethodPost, body, upstream, accept), user, box, func(resp *http.Response) error {
				if forgot = resp.StatusCode == http.StatusNotFound && !retried; forgot {
					return nil
				}
				return use(resp)
			})
		}
		switch {
		case forgot:
			if sess.agents.retire(ctx, box, upstream) {
				sess.reimage(box)
			}
		case retried || status.Code(err) != codes.NotFound:
			return box, upstream, err
		default:
			sess.reset(ctx, box, upstream)
		}
	}
}

// connect returns the box ref names, else the session's default box, also when it fails, and the session's agent session on it, opening one when it has none.
func (s *Server) connect(ctx context.Context, user string, sess *session, ref string) (string, string, error) {
	sess.mu.Lock()
	box, image, err := s.target(ctx, sess, ref)
	sess.mu.Unlock()
	if err != nil {
		return "", "", err
	}
	upstream, err := s.upstream(ctx, user, sess, box, image)
	return box, upstream, err
}

// upstream returns the session's agent session on box: the one held here, else the one another api process recorded, else a new one.
func (s *Server) upstream(ctx context.Context, user string, sess *session, box, image string) (string, error) {
	logger := log.WithFunc("controlmcp.Server.upstream")
	a := sess.agents
	a.mu.Lock()
	defer a.mu.Unlock()
	held := a.upstream[box]
	if held != "" && (a.held == nil || time.Since(a.read[box]) < rereadEvery) {
		return held, nil
	}
	if a.held != nil && !a.probe[box] {
		upstream, err := a.held.AgentSession(ctx, a.client, box)
		switch {
		case err != nil && held != "":
			logger.Error(ctx, err, "read the shared agent session again")
			a.read[box] = time.Now()
			return held, nil
		case err != nil:
			logger.Error(ctx, err, "read the shared agent session")
			return "", errSessionStore
		case upstream != "":
			a.upstream[box], a.read[box] = upstream, time.Now()
			return upstream, nil
		}
	}
	upstream, err := s.open(ctx, user, box, image, sess.params)
	if err != nil {
		return "", err
	}
	delete(a.probe, box)
	if a.held != nil {
		kept, err := a.held.PutAgentSession(ctx, a.client, box, upstream)
		if err != nil {
			logger.Error(ctx, err, "record the shared agent session")
			return "", errSessionStore
		}
		if kept != upstream {
			s.closeAgents(ctx, user, map[string]string{box: upstream})
		}
		upstream, a.read[box] = kept, time.Now()
	}
	a.upstream[box] = upstream
	return upstream, nil
}

// target returns the box ref names among the caller's, else the session's default box, found again once a grant no longer reaches it, and the image its agent runs; sess.mu is held.
func (s *Server) target(ctx context.Context, sess *session, ref string) (string, string, error) {
	if ref != "" {
		box, err := s.box(ctx, ref)
		if err != nil {
			return "", "", err
		}
		return box.GetId(), cmp.Or(box.GetImage(), box.GetLatestImage()), nil
	}
	if g := grantOf(ctx); g != nil && sess.box != "" && !g.Reaches(sess.box) {
		sess.box, sess.image = "", ""
	}
	if sess.box != "" && sess.image == "" {
		list, err := s.visible(ctx)
		if err != nil {
			return "", "", err
		}
		if i := slices.IndexFunc(list, func(b *controlv1.Box) bool { return b.GetId() == sess.box }); i >= 0 {
			sess.image = cmp.Or(list[i].GetImage(), list[i].GetLatestImage())
		} else {
			sess.box = ""
		}
	}
	if sess.box == "" {
		box, image, err := s.defaultBox(ctx)
		switch {
		case err != nil:
			return "", "", err
		case box == nil && grantOf(ctx) != nil:
			return "", "", errNoGrantBox
		case box == nil:
			return "", "", errNoBoxYet
		}
		sess.box, sess.image = box.GetId(), image
	}
	return sess.box, sess.image, nil
}

// open opens an agent session on box with the client's initialize, and records what the agent of image serves the first time an admin's box of it is reached.
func (s *Server) open(ctx context.Context, user, box, image string, params jsontext.Value) (string, error) {
	var (
		upstream string
		answer   struct {
			Result struct {
				Instructions string `json:"instructions"`
			} `json:"result"`
		}
	)
	if len(params) == 0 {
		params = defaultInitialize
	}
	hello, err := json.Marshal(request{JSONRPC: mcpwire.Version, ID: jsontext.Value(`0`), Method: methodInitialize, Params: params})
	if err != nil {
		return "", err
	}
	err = s.agents.Exchange(agentRequest(ctx, http.MethodPost, hello, "", acceptBoth), user, box, func(resp *http.Response) error {
		if upstream = resp.Header.Get(mcpwire.SessionHeader); resp.StatusCode != http.StatusOK || upstream == "" {
			return status.Errorf(codes.Unavailable, "the box agent answered initialize with %s", resp.Status)
		}
		return decodeAnswer(resp.Body, &answer)
	})
	if err != nil {
		return "", err
	}
	if owner, gerr := s.lists.Get(user); gerr != nil || !owner.Admin {
		return upstream, nil
	}
	logger := log.WithFunc("controlmcp.Server.open")
	family := boxes.Family(image)
	l, err := s.lists.ToolList(ctx, image, family)
	if err != nil || l.Image == image {
		logger.Error(ctx, err, "read the tool list of "+image)
		return upstream, nil
	}
	var list struct {
		Result struct {
			Tools jsontext.Value `json:"tools"`
		} `json:"result"`
	}
	err = s.agents.Exchange(agentRequest(ctx, http.MethodPost, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), upstream, acceptBoth), user, box, func(resp *http.Response) error {
		return decodeAnswer(resp.Body, &list)
	})
	if err != nil || len(list.Result.Tools) == 0 {
		logger.Warnf(ctx, "list the tools of image %s on box %s: %v", image, box, err)
		return upstream, nil
	}
	if err = s.lists.PutToolList(ctx, family, users.ToolList{Image: image, Tools: list.Result.Tools, Instructions: answer.Result.Instructions}); err != nil {
		logger.Error(ctx, err, "record the tool list of "+image)
	}
	return upstream, nil
}

// defaultBox returns the caller's default box, nil without one, and the image its tools come from: the box's, else the default image.
func (s *Server) defaultBox(ctx context.Context) (*controlv1.Box, string, error) {
	box, err := s.box(ctx, "")
	if err != nil {
		return nil, "", err
	}
	if image := cmp.Or(box.GetImage(), box.GetLatestImage()); image != "" {
		return box, image, nil
	}
	list, err := s.boxes.List(ctx, &controlv1.ListBoxesRequest{})
	if err != nil {
		return nil, "", err
	}
	return box, cmp.Or(list.GetOfferedImages()...), nil
}

// relay copies the agent's answer to the client as it arrives; into stream, which carries the wake's progress, it goes as events.
func relay(w http.ResponseWriter, resp *http.Response, stream *eventStream) error {
	if stream != nil && !strings.HasPrefix(resp.Header.Get("Content-Type"), mcpwire.EventStream) {
		data, err := readAnswer(resp.Body)
		if err != nil {
			return err
		}
		stream.write(mcpwire.Event(bytes.TrimSpace(data)))
		return nil
	}
	if stream == nil {
		for _, h := range []string{"Content-Type", "Cache-Control"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
	}
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil
			}
			_ = rc.Flush()
		}
		if err != nil {
			return nil
		}
	}
}

func readAnswer(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxAnswer+1))
	if err == nil && len(data) > maxAnswer {
		err = fmt.Errorf("the box agent's answer exceeds %d MiB", maxAnswer>>20)
	}
	return data, err
}

func decodeAnswer(r io.Reader, v any) error {
	data, err := readAnswer(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// takeBox takes the box argument out of a box tool call, returning the box it names, empty without one, and the body to send on.
func takeBox(body []byte, call *callParams) (string, []byte, error) {
	if !bytes.Contains(call.Arguments, boxField) {
		return "", body, nil
	}
	var args map[string]jsontext.Value
	if err := json.Unmarshal(call.Arguments, &args); err != nil || args["box"] == nil {
		return "", body, nil
	}
	var ref string
	if err := json.Unmarshal(args["box"], &ref); err != nil {
		return "", nil, errors.New("box must be a string")
	}
	delete(args, "box")
	var msg, params map[string]jsontext.Value
	if err := json.Unmarshal(body, &msg); err != nil {
		return "", nil, err
	}
	if err := json.Unmarshal(msg["params"], &params); err != nil {
		return "", nil, err
	}
	var err error
	if params["arguments"], err = json.Marshal(args, json.Deterministic(true)); err != nil {
		return "", nil, err
	}
	if msg["params"], err = json.Marshal(params, json.Deterministic(true)); err != nil {
		return "", nil, err
	}
	body, err = json.Marshal(msg, json.Deterministic(true))
	return ref, body, err
}

// withBoxArgument adds the box argument to a box tool's input schema; a tool it cannot read goes as it is.
func withBoxArgument(tool jsontext.Value) jsontext.Value {
	var t, schema, properties map[string]jsontext.Value
	if json.Unmarshal(tool, &t) != nil || json.Unmarshal(t["inputSchema"], &schema) != nil {
		return tool
	}
	if raw := schema["properties"]; raw != nil && json.Unmarshal(raw, &properties) != nil {
		return tool
	}
	if properties == nil {
		properties = map[string]jsontext.Value{}
	}
	properties["box"] = jsontext.Value(boxArgument)
	var err error
	if schema["properties"], err = json.Marshal(properties, json.Deterministic(true)); err != nil {
		return tool
	}
	if t["inputSchema"], err = json.Marshal(schema, json.Deterministic(true)); err != nil {
		return tool
	}
	out, err := json.Marshal(t, json.Deterministic(true))
	if err != nil {
		return tool
	}
	return out
}

func isControlTool(name string) bool {
	return slices.ContainsFunc(toolSpecs, func(t toolSpec) bool { return t.Name == name })
}

func toolNames(tools []jsontext.Value) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		var tool struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t, &tool)
		names = append(names, tool.Name)
	}
	return names
}

// failure is the tool result a box call that could not run answers.
func failure(err error) *toolResult {
	if refusal, ok := errors.AsType[toolError](err); ok {
		return errorText(refusal.Error())
	}
	return errorText(status.Convert(err).Message())
}

func sessionClient(caller, id string) string {
	return "session " + caller + "\n" + id
}

func grantClient(id string) string {
	return "grant " + id
}

func agentRequest(ctx context.Context, method string, body []byte, upstream, accept string) *http.Request {
	header := http.Header{"Content-Type": {"application/json"}, "Accept": {cmp.Or(accept, acceptBoth)}}
	if upstream != "" {
		header.Set(mcpwire.SessionHeader, upstream)
	}
	req := &http.Request{Method: method, URL: &url.URL{}, Header: header, Body: http.NoBody}
	if body != nil {
		req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	}
	return req.WithContext(ctx)
}
