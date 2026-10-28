package controlmcp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
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
	// maxInstructions is the Mac bridge's cap on a server's instructions, within every agent CLI's limit.
	maxInstructions = 2048
	acceptBoth      = "application/json, " + mcpwire.EventStream
	unifiedHead     = "These tools run on the user's cocoonbox, a cloud computer, not on this machine. box_* tools manage the user's boxes; the other tools act on the default box, the user's first, and wake it when it sleeps."

	errStillStarting toolError = "The box is still starting; call the tool again in a few seconds."
)

var (
	errNoBoxYet = status.Error(codes.Unavailable, "the user has no box yet")

	defaultInitialize = json.RawMessage(`{"protocolVersion":"` + mcpwire.Latest + `","capabilities":{},"clientInfo":{"name":"` + unifiedName + `"}}`)
)

// session is what the api keeps of one client session on RootPath: the client's initialize and the agent session it opened on the default box.
type session struct {
	params json.RawMessage
	used   time.Time

	mu       sync.Mutex
	box      string
	image    string
	upstream string
}

// reset drops the default box and its agent session while it still holds upstream, so the next call finds both again.
func (s *session) reset(upstream string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upstream == upstream {
		s.box, s.image, s.upstream = "", "", ""
	}
}

// openSession records a new client session of caller, dropping sessions idle for sessionIdle and caller's least recently used past maxSessions.
func (s *Server) openSession(caller, id string, params json.RawMessage) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keepSession(caller, id, &session{params: params})
}

// session returns caller's client session id, made anew when this api does not know it, as after a restart.
func (s *Server) session(caller, id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[caller+"\n"+id]; sess != nil {
		sess.used = time.Now()
		return sess
	}
	return s.keepSession(caller, id, &session{})
}

func (s *Server) keepSession(caller, id string, sess *session) *session {
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
	sess.used = now
	s.sessions[caller+"\n"+id] = sess
	return sess
}

// endSession forgets caller's client session id and closes its agent session.
func (s *Server) endSession(ctx context.Context, user, caller, id string) {
	s.mu.Lock()
	sess := s.sessions[caller+"\n"+id]
	delete(s.sessions, caller+"\n"+id)
	s.mu.Unlock()
	if sess == nil {
		return
	}
	sess.mu.Lock()
	box, upstream := sess.box, sess.upstream
	sess.mu.Unlock()
	if upstream != "" {
		_ = s.agents.Exchange(agentRequest(ctx, http.MethodDelete, nil, upstream, acceptBoth), user, box, func(*http.Response) error { return nil })
	}
}

// initializeUnified answers an initialize on RootPath, opening the agent session of the default box when it runs; the instructions add the box's own when they fit.
func (s *Server) initializeUnified(ctx context.Context, user string, sess *session) map[string]any {
	logger := log.WithFunc("controlmcp.Server.initializeUnified")
	result := initializeResult(sess.params, unifiedName)
	result["instructions"] = unifiedHead
	box, image, err := s.defaultBox(ctx)
	if err != nil {
		return result
	}
	if box.GetState() == controlv1.BoxState_BOX_STATE_RUNNING {
		sess.mu.Lock()
		sess.box, sess.image = box.GetId(), image
		sess.mu.Unlock()
		if _, _, err = s.connect(ctx, user, sess); err != nil {
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

// listTools lists the control tools, then the default box's tools as the agent of its image serves them, sleeping or not.
func (s *Server) listTools(ctx context.Context) (any, *mcpwire.Error) {
	_, image, err := s.defaultBox(ctx)
	if err != nil {
		return nil, &mcpwire.Error{Code: mcpwire.CodeInternal, Message: status.Convert(err).Message()}
	}
	var boxTools []json.RawMessage
	l, err := s.lists.ToolList(ctx, image, boxes.Family(image))
	if err == nil && len(l.Tools) != 0 {
		err = json.Unmarshal(l.Tools, &boxTools)
	}
	if err != nil {
		log.WithFunc("controlmcp.Server.listTools").Error(ctx, err, "read the tool list of "+image)
		return nil, &mcpwire.Error{Code: mcpwire.CodeInternal, Message: "reading the box tools failed"}
	}
	tools := make([]any, 0, len(toolSpecs)+len(boxTools))
	for _, t := range toolSpecs {
		tools = append(tools, t)
	}
	for _, t := range boxTools {
		tools = append(tools, t)
	}
	return map[string]any{toolsField: tools}, nil
}

// callBox runs a box tool on the session's default box; a box that is not running is woken first, within wakeBound.
func (s *Server) callBox(ctx context.Context, w http.ResponseWriter, user string, sess *session, id json.RawMessage, body []byte, accept string, call *callParams) {
	err := s.forward(ctx, user, sess, body, accept, func(resp *http.Response) error { relay(w, resp, nil); return nil })
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
	if err = s.wake(wakeCtx, sess, progress); err == nil {
		err = s.ready(wakeCtx, user, sess)
	}
	if stream != nil {
		stream.close()
	}
	switch {
	case err == nil:
		err = s.forward(ctx, user, sess, body, accept, func(resp *http.Response) error { relay(w, resp, stream); return nil })
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

// wake starts or wakes the session's default box, the user's first box made anew when there is none, and waits for it within ctx.
func (s *Server) wake(ctx context.Context, sess *session, progress progressFunc) error {
	sess.mu.Lock()
	box := sess.box
	sess.box, sess.image, sess.upstream = "", "", ""
	sess.mu.Unlock()
	_, err := s.ensure(ctx, box, true, progress)
	return err
}

// ready waits until the agent of the session's default box answers an initialize, or ctx ends.
func (s *Server) ready(ctx context.Context, user string, sess *session) error {
	for {
		_, _, err := s.connect(ctx, user, sess)
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

// forward sends body to the agent session of the default box, again on a new one once when the agent forgot the session or the box went.
func (s *Server) forward(ctx context.Context, user string, sess *session, body []byte, accept string, use func(*http.Response) error) error {
	for retried := false; ; retried = true {
		box, upstream, err := s.connect(ctx, user, sess)
		forgot := false
		if err == nil {
			err = s.agents.Exchange(agentRequest(ctx, http.MethodPost, body, upstream, accept), user, box, func(resp *http.Response) error {
				if forgot = resp.StatusCode == http.StatusNotFound && !retried; forgot {
					return nil
				}
				return use(resp)
			})
		}
		if !forgot && (retried || status.Code(err) != codes.NotFound) {
			return err
		}
		sess.reset(upstream)
	}
}

// connect returns the session's default box and its agent session, finding the box and opening the session when it has none.
func (s *Server) connect(ctx context.Context, user string, sess *session) (string, string, error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
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
	if sess.upstream == "" {
		upstream, err := s.open(ctx, user, sess.box, sess.image, sess.params)
		if err != nil {
			return "", "", err
		}
		sess.upstream = upstream
	}
	return sess.box, sess.upstream, nil
}

// open opens an agent session on box with the client's initialize, and records what the agent of image serves the first time an admin's box of it is reached.
func (s *Server) open(ctx context.Context, user, box, image string, params json.RawMessage) (string, error) {
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
	hello, err := marshal(request{JSONRPC: mcpwire.Version, ID: json.RawMessage(`0`), Method: methodInitialize, Params: params})
	if err != nil {
		return "", err
	}
	err = s.agents.Exchange(agentRequest(ctx, http.MethodPost, hello, "", acceptBoth), user, box, func(resp *http.Response) error {
		if upstream = resp.Header.Get(mcpwire.SessionHeader); resp.StatusCode != http.StatusOK || upstream == "" {
			return status.Errorf(codes.Unavailable, "the box agent answered initialize with %s", resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(&answer)
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
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	err = s.agents.Exchange(agentRequest(ctx, http.MethodPost, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), upstream, acceptBoth), user, box, func(resp *http.Response) error {
		return json.NewDecoder(resp.Body).Decode(&list)
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
func relay(w http.ResponseWriter, resp *http.Response, stream *eventStream) {
	if stream != nil && !strings.HasPrefix(resp.Header.Get("Content-Type"), mcpwire.EventStream) {
		if data, err := io.ReadAll(resp.Body); err == nil {
			stream.write(mcpwire.Event(bytes.TrimSpace(data)))
		}
		return
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
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// failure is the tool result a box call that could not run answers.
func failure(err error) *toolResult {
	if refusal, ok := errors.AsType[toolError](err); ok {
		return errorText(refusal.Error())
	}
	return errorText(status.Convert(err).Message())
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
