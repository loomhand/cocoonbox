package controlmcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/boxes"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/mcpwire"
	"github.com/loomhand/cocoonbox/api/oauth"
	"github.com/loomhand/cocoonbox/api/users"
)

const (
	grantToken   = "cba_grant"
	boxTools     = `[{"name":"shell","description":"Run a command.","inputSchema":{"type":"object"}},{"name":"gui_plan","description":"Run a plan.","inputSchema":{"type":"object"},"_meta":{"anthropic/alwaysLoad":true}}]`
	agentGuide   = "These tools act on a cloud computer."
	initialize   = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"claude-code"}}}`
	callShell    = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"shell","arguments":{"command":"id"}}}`
	listRequest  = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	shellAnswer  = "ran shell on "
	controlTools = 9
)

func TestRootListsControlAndBoxToolsWhileTheBoxSleeps(t *testing.T) {
	srv, fake, agents, lists := newUnifiedServer(t)
	fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
	lists.put("img:0", users.ToolList{Image: "img:0", Tools: json.RawMessage(`[{"name":"shell"}]`)})
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: json.RawMessage(boxTools), Instructions: agentGuide})

	rec := serveRoot(srv, http.MethodPost, aliceToken, "", initialize)
	init := decode(t, rec)
	info, _ := init.Result["serverInfo"].(map[string]any)
	if info["name"] != "cocoonbox" || init.Result["instructions"] != unifiedHead+"\n\n"+agentGuide || rec.Header().Get(mcpwire.SessionHeader) == "" {
		t.Fatalf("initialize = %+v", init.Result)
	}
	tools, _ := decode(t, serveRoot(srv, http.MethodPost, aliceToken, rec.Header().Get(mcpwire.SessionHeader), listRequest)).Result["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if want := []string{"box_list", "box_status", "box_ensure", "box_create", "box_hibernate", "box_save", "box_upgrade", "box_delete", "usage", "shell", "gui_plan"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	if meta, _ := tools[len(tools)-1].(map[string]any)["_meta"].(map[string]any); meta["anthropic/alwaysLoad"] != true {
		t.Errorf("gui_plan lost its _meta: %v", tools[len(tools)-1])
	}
	if sent := agents.recorded(); len(sent) != 0 {
		t.Errorf("listing a sleeping box's tools reached its agent: %v", sent)
	}
	if calls := fake.recorded(); slices.ContainsFunc(calls, func(c fakeCall) bool { return c.method == "Ensure" }) {
		t.Errorf("listing woke the box: %v", calls)
	}
}

func TestRootCallWakesTheSleepingDefaultBox(t *testing.T) {
	srv, fake, agents, lists := newUnifiedServer(t)
	fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)

	rec := serveRoot(srv, http.MethodPost, aliceToken, session, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"shell","arguments":{"command":"id"},"_meta":{"progressToken":"tok"}}}`)
	if rec.Header().Get("Content-Type") != mcpwire.EventStream {
		t.Fatalf("content type %q: %s", rec.Header().Get("Content-Type"), rec.Body)
	}
	msgs := sseMessages(t, rec.Body.String())
	if len(msgs) != 3 || msgs[0]["params"].(map[string]any)["message"] != "waking: restoring memory snapshot" || msgs[1]["params"].(map[string]any)["message"] != "done: running" {
		t.Fatalf("stream = %v", msgs)
	}
	if text := callText(t, msgs[2]); msgs[2]["id"] != float64(7) || text != shellAnswer+boxes.BoxID("alice") {
		t.Fatalf("answer = %v", msgs[2])
	}
	if !slices.Contains(fake.recorded(), fakeCall{method: "Ensure", user: "alice", box: boxes.BoxID("alice")}) {
		t.Errorf("calls = %v, want an Ensure of alice's box", fake.recorded())
	}
	if want := []string{"initialize", "tools/list", "tools/call"}; !slices.Equal(agents.recorded(), want) {
		t.Errorf("agent saw %v, want %v", agents.recorded(), want)
	}
	if l := lists.get("img:1"); string(l.Tools) != boxTools || l.Instructions != agentGuide {
		t.Errorf("recorded tool list = %+v", l)
	}
}

func TestRootWakePastTheBoundAsksForARetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, fake, agents, _ := newUnifiedServer(t)
		fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
		agents.mu.Lock()
		agents.deaf = true
		agents.mu.Unlock()
		if got := refusal(t, decode(t, serveRoot(srv, http.MethodPost, aliceToken, "s1", callShell))); got != string(errStillStarting) {
			t.Fatalf("call past the bound = %q", got)
		}
		if !slices.Contains(fake.recorded(), fakeCall{method: "Ensure", user: "alice", box: boxes.BoxID("alice")}) {
			t.Errorf("calls = %v, want an Ensure", fake.recorded())
		}
	})
}

func TestRootCallOfAGrantWithoutABox(t *testing.T) {
	srv, fake, agents, lists := newUnifiedServer(t)
	srv.EnableGrants(fakeGrants{})
	lists.put(latestImage, users.ToolList{Image: latestImage, Tools: json.RawMessage(boxTools)})

	tools, _ := decode(t, serveRoot(srv, http.MethodPost, grantToken, "", listRequest)).Result["tools"].([]any)
	if len(tools) != controlTools+2 {
		t.Fatalf("a grant without a box lists %d tools, want the control tools and the default image's", len(tools))
	}
	if got := refusal(t, decode(t, serveRoot(srv, http.MethodPost, grantToken, "s1", callShell))); got != string(errNoGrantBox) {
		t.Fatalf("box call of a grant without a box = %q", got)
	}
	if len(agents.recorded()) != 0 || slices.ContainsFunc(fake.recorded(), func(c fakeCall) bool { return c.method == "Ensure" }) {
		t.Errorf("the call reached a box: agent %v, boxes %v", agents.recorded(), fake.recorded())
	}
}

func TestRootCallOnARunningBoxIsOneAgentRoundTrip(t *testing.T) {
	srv, fake, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: json.RawMessage(boxTools), Instructions: agentGuide})
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	if want := []string{"initialize"}; !slices.Equal(agents.recorded(), want) {
		t.Fatalf("initialize on a running box sent %v, want %v", agents.recorded(), want)
	}

	for i := range 2 {
		if text := callText(t, map[string]any{"result": decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell)).Result}); text != shellAnswer+boxes.BoxID("alice") {
			t.Fatalf("call %d = %q", i, text)
		}
	}
	if want := []string{"initialize", "tools/call", "tools/call"}; !slices.Equal(agents.recorded(), want) {
		t.Fatalf("two calls sent %v, want %v", agents.recorded(), want)
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Errorf("calls on a running box reached Boxes: %v", calls)
	}

	agents.forget()
	if text := callText(t, map[string]any{"result": decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell)).Result}); text != shellAnswer+boxes.BoxID("alice") {
		t.Fatalf("call after the agent forgot the session = %q", text)
	}
	if rec := serveRoot(srv, http.MethodDelete, aliceToken, session, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", rec.Code)
	}
	if want := []string{"initialize", "tools/call", "tools/call", "tools/call", "initialize", "tools/call", "DELETE"}; !slices.Equal(agents.recorded(), want) {
		t.Fatalf("agent saw %v, want %v", agents.recorded(), want)
	}
}

func TestRootCallLostInFlightIsNotSentAgain(t *testing.T) {
	srv, fake, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: json.RawMessage(boxTools)})
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	agents.mu.Lock()
	agents.lost = true
	agents.mu.Unlock()
	if got := refusal(t, decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell))); got != "box agent unreachable" {
		t.Fatalf("call lost in flight = %q", got)
	}
	if want := []string{"initialize", "tools/call"}; !slices.Equal(agents.recorded(), want) || len(fake.recorded()) != 0 {
		t.Fatalf("agent saw %v and Boxes %v, want %v and nothing", agents.recorded(), fake.recorded(), want)
	}
}

func TestRootRecordsToolListsOnlyFromAnAdminsBox(t *testing.T) {
	srv, _, agents, lists := newUnifiedServer(t)
	serveRoot(srv, http.MethodPost, bobToken, "", initialize)
	if want := []string{"initialize"}; !slices.Equal(agents.recorded(), want) || lists.get("img:1").Image != "" {
		t.Fatalf("a user's box: agent saw %v, recorded %+v", agents.recorded(), lists.get("img:1"))
	}
	serveRoot(srv, http.MethodPost, aliceToken, "", initialize)
	if want := []string{"initialize", "initialize", "tools/list"}; !slices.Equal(agents.recorded(), want) || string(lists.get("img:1").Tools) != boxTools {
		t.Fatalf("an admin's box: agent saw %v, recorded %+v", agents.recorded(), lists.get("img:1"))
	}
}

func TestRootParallelCallsOnAnExpiredSessionOpenOneNew(t *testing.T) {
	srv, _, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: json.RawMessage(boxTools)})
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	agents.forget()
	agents.mu.Lock()
	agents.hold = &sync.WaitGroup{}
	agents.hold.Add(2)
	agents.mu.Unlock()
	var wg sync.WaitGroup
	texts := make([]string, 2)
	for i := range texts {
		wg.Go(func() {
			rec := serveRoot(srv, http.MethodPost, aliceToken, session, callShell)
			var reply rpcReply
			if json.Unmarshal(rec.Body.Bytes(), &reply) == nil {
				if content, _ := reply.Result["content"].([]any); len(content) == 1 {
					texts[i], _ = content[0].(map[string]any)["text"].(string)
				}
			}
		})
	}
	wg.Wait()
	if want := shellAnswer + boxes.BoxID("alice"); texts[0] != want || texts[1] != want {
		t.Fatalf("parallel calls = %q", texts)
	}
	if n := len(slices.DeleteFunc(agents.recorded(), func(m string) bool { return m != "initialize" })); n != 2 {
		t.Fatalf("agent saw %v, want one new initialize", agents.recorded())
	}
}

func TestRootCallAfterTheDefaultBoxWentReachesTheNextOne(t *testing.T) {
	srv, fake, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: json.RawMessage(boxTools)})
	agents.mu.Lock()
	agents.lost = true
	agents.mu.Unlock()
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	agents.mu.Lock()
	agents.lost = false
	agents.mu.Unlock()
	fake.mu.Lock()
	fake.gone["alice"] = true
	fake.extra["alice"] = []*controlv1.Box{{Id: "cb-next", Name: "next", State: controlv1.BoxState_BOX_STATE_RUNNING, Image: "img:1"}}
	fake.mu.Unlock()
	if text := callText(t, map[string]any{"result": decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell)).Result}); text != shellAnswer+"cb-next" {
		t.Fatalf("call after the default box went = %q", text)
	}
}

func TestRootBoxArgumentPicksTheBoxAndListsInTheSchema(t *testing.T) {
	srv, fake, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: json.RawMessage(boxTools)})
	fake.mu.Lock()
	fake.extra["alice"] = []*controlv1.Box{{Id: "cb-next", Name: "next", State: controlv1.BoxState_BOX_STATE_RUNNING, Image: "img:1"}}
	fake.mu.Unlock()
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)

	tools, _ := decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, listRequest)).Result["tools"].([]any)
	for _, tool := range tools[controlTools:] {
		schema, _ := tool.(map[string]any)["inputSchema"].(map[string]any)
		if prop, _ := schema["properties"].(map[string]any)["box"].(map[string]any); prop["type"] != "string" {
			t.Errorf("box tool without the box argument: %v", tool)
		}
	}
	call := func(box string) string {
		body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"shell","arguments":{"command":"id","box":"` + box + `"}}}`
		return callText(t, map[string]any{"result": decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, body)).Result})
	}
	if text := call("next"); text != shellAnswer+"cb-next" {
		t.Fatalf("call on box next = %q", text)
	}
	if text := call(""); text != shellAnswer+boxes.BoxID("alice") {
		t.Fatalf("call with an empty box = %q", text)
	}
	if text := call("nope"); text != `You have no box "nope"; box_list lists your boxes.` {
		t.Fatalf("call on a box the caller lacks = %q", text)
	}
	if text := callText(t, map[string]any{"result": decode(t, serveRoot(srv, http.MethodPost, bobToken, "", strings.Replace(callShell, `"id"}`, `"id","box":"cb-next"}`, 1))).Result}); !strings.Contains(text, `no box "cb-next"`) {
		t.Fatalf("bob's call on alice's box = %q", text)
	}
	if want := []string{"initialize", "initialize", "tools/call", "tools/call"}; !slices.Equal(agents.recorded(), want) {
		t.Fatalf("agent saw %v, want one agent session per box", agents.recorded())
	}
	if slices.ContainsFunc(agents.bodies(), func(b string) bool { return strings.Contains(b, `"box"`) }) {
		t.Errorf("the box argument reached the agent: %v", agents.bodies())
	}
}

func TestRootDefaultOfAGrantIsItsFirstChosenBox(t *testing.T) {
	srv, fake, _, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: json.RawMessage(boxTools)})
	fake.mu.Lock()
	fake.extra["alice"] = []*controlv1.Box{
		{Id: "cb-made", Name: "made", State: controlv1.BoxState_BOX_STATE_RUNNING, Image: "img:1"},
		{Id: "cb-picked", Name: "picked", State: controlv1.BoxState_BOX_STATE_RUNNING, Image: "img:1"},
	}
	fake.mu.Unlock()
	srv.EnableGrants(fakeGrants{boxes: []string{"cb-made"}, chosen: []string{"cb-picked"}})
	if text := callText(t, map[string]any{"result": decode(t, serveRoot(srv, http.MethodPost, grantToken, "s1", callShell)).Result}); text != shellAnswer+"cb-picked" {
		t.Fatalf("a grant's default call = %q, want its chosen box", text)
	}
	srv.EnableGrants(fakeGrants{boxes: []string{"cb-made"}})
	if text := callText(t, map[string]any{"result": decode(t, serveRoot(srv, http.MethodPost, grantToken, "s1", callShell)).Result}); text != shellAnswer+"cb-made" {
		t.Fatalf("the default call once the box is no longer chosen = %q, want the created box", text)
	}
}

func TestRootGrantSharesOneAgentSessionPerBoxAcrossClientSessions(t *testing.T) {
	srv, _, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: json.RawMessage(boxTools)})
	srv.EnableGrants(fakeGrants{boxes: []string{boxes.BoxID("alice")}})
	for i := range 3 {
		session := serveRoot(srv, http.MethodPost, grantToken, "", initialize).Header().Get(mcpwire.SessionHeader)
		if text := callText(t, map[string]any{"result": decode(t, serveRoot(srv, http.MethodPost, grantToken, session, callShell)).Result}); text != shellAnswer+boxes.BoxID("alice") {
			t.Fatalf("call %d = %q", i, text)
		}
		if rec := serveRoot(srv, http.MethodDelete, grantToken, session, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE = %d", rec.Code)
		}
	}
	if want := []string{"initialize", "tools/call", "tools/call", "tools/call"}; !slices.Equal(agents.recorded(), want) {
		t.Fatalf("a grant re-initializing before each call: agent saw %v, want %v", agents.recorded(), want)
	}
	if calls := agents.calledOn(); len(slices.Compact(calls)) != 1 {
		t.Fatalf("the grant's calls went to agent sessions %v, want one", calls)
	}

	for range 2 {
		session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
		decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell))
	}
	if n := len(slices.DeleteFunc(agents.recorded(), func(m string) bool { return m != "initialize" })); n != 3 {
		t.Fatalf("two client sessions of a user token opened %d agent sessions in all, want 1 + 2", n)
	}
}

func TestRootKeepsAtMostMaxSessionsPerCaller(t *testing.T) {
	srv, _, _, _ := newUnifiedServer(t)
	first := srv.openSession(t.Context(), "alice", "s0", nil)
	srv.mu.Lock()
	first.used = first.used.Add(-time.Hour)
	srv.mu.Unlock()
	for i := range maxSessions {
		srv.openSession(t.Context(), "alice", "s"+string(rune('a'+i)), nil)
	}
	srv.session(t.Context(), "bob", "b0")
	srv.mu.Lock()
	defer srv.mu.Unlock()
	alice := 0
	for key, sess := range srv.sessions {
		if strings.HasPrefix(key, "alice\n") {
			alice++
		}
		if sess == first {
			t.Errorf("the least recently used session %s stayed", key)
		}
	}
	if alice != maxSessions || len(srv.sessions) != maxSessions+1 {
		t.Fatalf("alice keeps %d sessions of %d in all, want %d and %d", alice, len(srv.sessions), maxSessions, maxSessions+1)
	}
}

func TestRootTransport(t *testing.T) {
	srv, _, _, _ := newUnifiedServer(t)
	for _, tc := range []struct {
		name, method, token string
		want                int
	}{
		{"no token", http.MethodPost, "", http.StatusUnauthorized},
		{"get stream", http.MethodGet, aliceToken, http.StatusMethodNotAllowed},
		{"unknown session delete", http.MethodDelete, aliceToken, http.StatusNoContent},
	} {
		if rec := serveRoot(srv, tc.method, tc.token, "", listRequest); rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
	if resp := post(t, srv, aliceToken, callShell); resp.Error == nil || resp.Error.Code != mcpwire.CodeInvalidParams {
		t.Errorf("a box tool on the control path = %+v", resp)
	}
}

type fakeGrants struct {
	boxes  []string
	chosen []string
}

func (g fakeGrants) Authenticate(context.Context, string) (oauth.Grant, error) {
	return oauth.Grant{ID: "g1", User: "alice", MaxBoxes: 2, MaxRunning: 1, Boxes: g.boxes, Chosen: g.chosen}, nil
}

func (g fakeGrants) Boxes(context.Context, string) ([]string, error) { return g.boxes, nil }

func (fakeGrants) AddBox(context.Context, string, string) error { return nil }

func (fakeGrants) Challenge(bool) string { return "Bearer" }

// fakeAgents answers MCP like the agent of a running box of fakeBoxes; a deaf one never answers, one that lost the connection drops every request it got, and hold keeps each answer to an expired session until all it counts were made.
type fakeAgents struct {
	boxes *fakeBoxes

	mu       sync.Mutex
	sent     []string
	posted   []string
	called   []string
	sessions map[string]bool
	deaf     bool
	lost     bool
	hold     *sync.WaitGroup
}

func (a *fakeAgents) Exchange(req *http.Request, user, box string, use func(*http.Response) error) error {
	a.boxes.mu.Lock()
	state, owned := a.boxes.states[user], box == boxes.BoxID(user) && !a.boxes.gone[user]
	if i := slices.IndexFunc(a.boxes.extra[user], func(b *controlv1.Box) bool { return b.GetId() == box }); i >= 0 {
		state, owned = a.boxes.extra[user][i].GetState(), true
	}
	a.boxes.mu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case !owned:
		return boxes.ErrNoBox
	case state != controlv1.BoxState_BOX_STATE_RUNNING || a.deaf:
		return status.Error(codes.Unavailable, "box is hibernated; call box_ensure to bring it up")
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	body, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(body, &msg)
	a.posted = append(a.posted, string(body))
	session := req.Header.Get(mcpwire.SessionHeader)
	if req.Method == http.MethodDelete {
		a.sent = append(a.sent, req.Method)
		delete(a.sessions, session)
		return use(answer(http.StatusNoContent, "", ""))
	}
	a.sent = append(a.sent, msg.Method)
	if a.lost {
		return status.Error(codes.Unknown, "box agent unreachable")
	}
	switch {
	case msg.Method == "initialize":
		session = "up" + string(rune('a'+len(a.sent)))
		a.sessions[session] = true
		return use(answer(http.StatusOK, session, `{"jsonrpc":"2.0","id":0,"result":{"instructions":"`+agentGuide+`"}}`))
	case !a.sessions[session]:
		if a.hold != nil {
			a.mu.Unlock()
			a.hold.Done()
			a.hold.Wait()
			a.mu.Lock()
		}
		return use(answer(http.StatusNotFound, "", `{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"unknown or expired session"}}`))
	case msg.Method == "tools/list":
		return use(answer(http.StatusOK, "", `{"jsonrpc":"2.0","id":1,"result":{"tools":`+boxTools+`}}`))
	default:
		a.called = append(a.called, session)
		return use(answer(http.StatusOK, "", `{"jsonrpc":"2.0","id":`+string(msg.ID)+`,"result":{"content":[{"type":"text","text":"`+shellAnswer+box+`"}]}}`))
	}
}

func (a *fakeAgents) forget() {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.sessions)
}

func (a *fakeAgents) calledOn() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.called)
}

func (a *fakeAgents) bodies() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.posted)
}

func (a *fakeAgents) recorded() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.sent)
}

type fakeLists struct {
	mu    sync.Mutex
	lists map[string]users.ToolList
}

func (f *fakeLists) ToolList(_ context.Context, image, family string) (users.ToolList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.lists[image]; ok {
		return l, nil
	}
	for name, l := range f.lists {
		if boxes.Family(name) == family {
			return l, nil
		}
	}
	return users.ToolList{}, nil
}

func (f *fakeLists) PutToolList(_ context.Context, _ string, l users.ToolList) error {
	f.put(l.Image, l)
	return nil
}

func (f *fakeLists) Get(id string) (users.User, error) {
	return users.User{ID: id, Admin: id == "alice"}, nil
}

func (f *fakeLists) put(image string, l users.ToolList) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists[image] = l
}

func (f *fakeLists) get(image string) users.ToolList {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists[image]
}

func newUnifiedServer(t *testing.T) (*Server, *fakeBoxes, *fakeAgents, *fakeLists) {
	t.Helper()
	srv, fake := newTestServer(t)
	agents := &fakeAgents{boxes: fake, sessions: map[string]bool{}}
	lists := &fakeLists{lists: map[string]users.ToolList{}}
	srv.EnableBoxTools(agents, lists)
	return srv, fake, agents, lists
}

func serveRoot(srv *Server, method, token, session, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, RootPath, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		req.Header.Set(mcpwire.SessionHeader, session)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func answer(code int, session, body string) *http.Response {
	header := http.Header{"Content-Type": {"application/json"}}
	if session != "" {
		header.Set(mcpwire.SessionHeader, session)
	}
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func callText(t *testing.T, msg map[string]any) string {
	t.Helper()
	result, _ := msg["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("not a tool result: %v", msg)
	}
	return content[0].(map[string]any)["text"].(string)
}
