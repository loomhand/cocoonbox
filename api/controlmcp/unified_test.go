package controlmcp

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
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

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/boxes"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
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
	navigate     = `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"browser_navigate","arguments":{"url":"https://example.com"}}}`
	snapshot     = `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"browser_snapshot","arguments":{}}}`
	accountTools = `[{"name":"mock_list_items","annotations":{"readOnlyHint":true}},{"name":"mock_create_item","annotations":{"destructiveHint":true}},{"name":"shell"},{"name":"box_list"}]`
	callAccount  = `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"mock_list_items","arguments":{"q":"x"}}}`
)

func TestRootListsControlAndBoxToolsWhileTheBoxSleeps(t *testing.T) {
	srv, fake, agents, lists := newUnifiedServer(t)
	fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
	lists.put("img:0", users.ToolList{Image: "img:0", Tools: jsontext.Value(`[{"name":"shell"}]`)})
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools), Instructions: agentGuide})

	rec := serveRoot(srv, http.MethodPost, aliceToken, "", initialize)
	init := decode(t, rec)
	info, _ := init.Result["serverInfo"].(map[string]any)
	if info["name"] != "cocoonbox" || init.Result["instructions"] != unifiedHead+"\n\n"+agentGuide || rec.Header().Get(mcpwire.SessionHeader) == "" {
		t.Fatalf("initialize = %+v", init.Result)
	}
	tools, _ := decode(t, serveRoot(srv, http.MethodPost, aliceToken, rec.Header().Get(mcpwire.SessionHeader), listRequest)).Result["tools"].([]any)
	if names, want := listedNames(tools), []string{"box_list", "box_status", "box_ensure", "box_create", "box_hibernate", "box_save", "box_upgrade", "box_delete", "usage", "shell", "gui_plan"}; !slices.Equal(names, want) {
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
	if text := callText(t, msgs[2]["result"]); msgs[2]["id"] != float64(7) || text != shellAnswer+boxes.BoxID("alice") {
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

func TestABoxAgentAnswerPastTheCapFailsTheCallInsteadOfBeingReadWhole(t *testing.T) {
	for _, method := range []string{"initialize", "tools/list", "tools/call"} {
		srv, fake, agents, _ := newUnifiedServer(t)
		fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
		agents.set(func(a *fakeAgents) { a.oversize = method })
		session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
		rec := serveRoot(srv, http.MethodPost, aliceToken, session, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"shell","arguments":{"command":"id"},"_meta":{"progressToken":"tok"}}}`)
		msgs := sseMessages(t, rec.Body.String())
		last := msgs[len(msgs)-1]
		if method == "tools/list" {
			if text := callText(t, last["result"]); text != shellAnswer+boxes.BoxID("alice") {
				t.Errorf("an oversized tools/list failed the call: %v", last)
			}
			continue
		}
		if text := callText(t, last["result"]); !strings.Contains(text, "exceeds 32 MiB") {
			t.Errorf("oversized %s answer = %v, want an error naming the cap", method, last)
		}
	}
}

func TestRootWakePastTheBoundAsksForARetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, fake, agents, _ := newUnifiedServer(t)
		fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
		agents.set(func(a *fakeAgents) { a.deaf = true })
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
	lists.put(latestImage, users.ToolList{Image: latestImage, Tools: jsontext.Value(boxTools)})

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
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools), Instructions: agentGuide})
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	if want := []string{"initialize"}; !slices.Equal(agents.recorded(), want) {
		t.Fatalf("initialize on a running box sent %v, want %v", agents.recorded(), want)
	}

	for i := range 2 {
		if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell)).Result); text != shellAnswer+boxes.BoxID("alice") {
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
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell)).Result); text != shellAnswer+boxes.BoxID("alice") {
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
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	agents.set(func(a *fakeAgents) { a.lost = true })
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
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	agents.forget()
	agents.set(func(a *fakeAgents) {
		a.hold = &sync.WaitGroup{}
		a.hold.Add(2)
	})
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
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	agents.set(func(a *fakeAgents) { a.lost = true })
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	agents.set(func(a *fakeAgents) { a.lost = false })
	fake.mu.Lock()
	fake.gone["alice"] = true
	fake.extra["alice"] = []*controlv1.Box{{Id: "cb-next", Name: "next", State: controlv1.BoxState_BOX_STATE_RUNNING, Image: "img:1"}}
	fake.mu.Unlock()
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell)).Result); text != shellAnswer+"cb-next" {
		t.Fatalf("call after the default box went = %q", text)
	}
}

func TestRootBoxArgumentPicksTheBoxAndListsInTheSchema(t *testing.T) {
	srv, fake, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
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
		return callText(t, decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, body)).Result)
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
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, bobToken, "", strings.Replace(callShell, `"id"}`, `"id","box":"cb-next"}`, 1))).Result); !strings.Contains(text, `no box "cb-next"`) {
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
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	fake.mu.Lock()
	fake.extra["alice"] = []*controlv1.Box{
		{Id: "cb-made", Name: "made", State: controlv1.BoxState_BOX_STATE_RUNNING, Image: "img:1"},
		{Id: "cb-picked", Name: "picked", State: controlv1.BoxState_BOX_STATE_RUNNING, Image: "img:1"},
	}
	fake.mu.Unlock()
	srv.EnableGrants(fakeGrants{boxes: []string{"cb-made"}, chosen: []string{"cb-picked"}})
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, grantToken, "s1", callShell)).Result); text != shellAnswer+"cb-picked" {
		t.Fatalf("a grant's default call = %q, want its chosen box", text)
	}
	srv.EnableGrants(fakeGrants{boxes: []string{"cb-made"}})
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, grantToken, "s1", callShell)).Result); text != shellAnswer+"cb-made" {
		t.Fatalf("the default call once the box is no longer chosen = %q, want the created box", text)
	}
}

func TestRootGrantSharesOneAgentSessionPerBoxAcrossClientSessions(t *testing.T) {
	srv, _, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	srv.EnableGrants(fakeGrants{boxes: []string{boxes.BoxID("alice")}})
	for i := range 3 {
		session := serveRoot(srv, http.MethodPost, grantToken, "", initialize).Header().Get(mcpwire.SessionHeader)
		if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, grantToken, session, callShell)).Result); text != shellAnswer+boxes.BoxID("alice") {
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

func TestRootWakeKeepsTheSessionsDefaultBox(t *testing.T) {
	srv, fake, _, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	made := boxes.BoxID("alice")
	srv.EnableGrants(fakeGrants{boxes: []string{made}})
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, grantToken, "s1", callShell)).Result); text != shellAnswer+made {
		t.Fatalf("first call = %q", text)
	}
	fake.mu.Lock()
	fake.extra["alice"] = []*controlv1.Box{{Id: "cb-picked", Name: "picked", State: controlv1.BoxState_BOX_STATE_RUNNING, Image: "img:1"}}
	fake.mu.Unlock()
	srv.EnableGrants(fakeGrants{boxes: []string{made}, chosen: []string{"cb-picked"}})
	fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, grantToken, "s1", callShell)).Result); text != shellAnswer+made {
		t.Fatalf("the call that woke the session's default box ran as %q, want on %s", text, made)
	}
}

func TestRootWakeDropsOnlyTheStaleAgentSession(t *testing.T) {
	srv, fake, _, _ := newUnifiedServer(t)
	box := boxes.BoxID("alice")
	fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
	sess := srv.openSession(t.Context(), "alice", "s1", nil)
	sess.agents.upstream[box] = "newer"
	ctx := auth.WithUser(t.Context(), "alice")
	if err := srv.wake(ctx, sess, box, "older", func(string) {}); err != nil || sess.agents.upstream[box] != "newer" {
		t.Fatalf("a wake that saw an older agent session: %v, left %q, want the newer one kept", err, sess.agents.upstream[box])
	}
	if err := srv.wake(ctx, sess, box, "newer", func(string) {}); err != nil || sess.agents.upstream[box] != "" {
		t.Fatalf("a wake that saw the held agent session: %v, left %q, want it dropped", err, sess.agents.upstream[box])
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

func TestRootServesTheToolsOfTheAccountsAGrantReaches(t *testing.T) {
	srv, _, _, lists := newUnifiedServer(t)
	lists.put(latestImage, users.ToolList{Image: latestImage, Tools: jsontext.Value(boxTools)})
	accounts := newFakeConnectors()
	srv.EnableConnectors(accounts)

	names := func(token string) []string {
		tools, _ := decode(t, serveRoot(srv, http.MethodPost, token, "", listRequest)).Result["tools"].([]any)
		return listedNames(tools)
	}
	srv.EnableGrants(fakeGrants{allAccounts: true})
	if got := names(grantToken); len(got) != controlTools+4 || !slices.Equal(got[controlTools:], []string{"shell", "gui_plan", "mock_list_items", "mock_create_item"}) {
		t.Fatalf("a grant with an account lists %v, want the control and box tools, then the account's tools a control or box tool does not shadow", got)
	}
	if got := accounts.listed(); !slices.Equal(got, []string{"alice true"}) {
		t.Errorf("listed the tools of %v", got)
	}
	if got := names(aliceToken); slices.Contains(got, "mock_list_items") {
		t.Errorf("the user token lists %v, want no account tools", got)
	}
	srv.EnableGrants(fakeGrants{})
	if got := names(grantToken); slices.Contains(got, "mock_list_items") || len(accounts.listed()) != 1 {
		t.Errorf("a grant without accounts lists %v and asked Connect %v", got, accounts.listed())
	}
}

func TestRootCallOfAnAccountToolGoesToConnectAndWakesNoBox(t *testing.T) {
	srv, fake, agents, lists := newUnifiedServer(t)
	fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
	lists.put(latestImage, users.ToolList{Image: latestImage, Tools: jsontext.Value(boxTools)})
	accounts := newFakeConnectors()
	srv.EnableConnectors(accounts)
	srv.EnableGrants(fakeGrants{boxes: []string{boxes.BoxID("alice")}, allAccounts: true})

	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, grantToken, "s1", callAccount)).Result); text != "mock_list_items done" {
		t.Fatalf("account call = %q", text)
	}
	if got := accounts.calledWith(); !slices.Equal(got, []string{`alice Agent true mock_list_items {"q":"x"}`}) {
		t.Errorf("Connect calls = %v", got)
	}
	if len(agents.recorded()) != 0 || slices.ContainsFunc(fake.recorded(), func(c fakeCall) bool { return c.method == "Ensure" }) {
		t.Errorf("the account call reached a box: agent %v, boxes %v", agents.recorded(), fake.recorded())
	}
	accounts.owned = nil
	serveRoot(srv, http.MethodPost, grantToken, "s1", callAccount)
	if len(accounts.calledWith()) != 1 || !slices.ContainsFunc(fake.recorded(), func(c fakeCall) bool { return c.method == "Ensure" }) {
		t.Errorf("a tool of an account the user no longer has went to Connect %v, not the box %v", accounts.calledWith(), fake.recorded())
	}
}

func TestAccountRoutingErrorsDoNotWakeOrCallABox(t *testing.T) {
	for _, source := range []string{"connector", "box tools"} {
		t.Run(source, func(t *testing.T) {
			srv, fake, agents, lists := newUnifiedServer(t)
			fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
			lists.put(latestImage, users.ToolList{Image: latestImage, Tools: jsontext.Value(boxTools)})
			accounts := newFakeConnectors()
			srv.EnableConnectors(accounts)
			srv.EnableGrants(fakeGrants{boxes: []string{boxes.BoxID("alice")}, allAccounts: true})
			if source == "connector" {
				accounts.err = errors.New("connector query failed")
			} else {
				lists.err = errors.New("tool list query failed")
			}
			resp := decode(t, serveRoot(srv, http.MethodPost, grantToken, "s1", callAccount))
			if resp.Error == nil || resp.Error.Code != mcpwire.CodeInternal {
				t.Errorf("routing failure = %+v, want an internal error", resp)
			}
			if len(agents.recorded()) != 0 || slices.ContainsFunc(fake.recorded(), func(c fakeCall) bool { return c.method == "Ensure" }) || len(accounts.calledWith()) != 0 {
				t.Errorf("routing failure ran a tool or woke a box: agent %v, boxes %v, accounts %v", agents.recorded(), fake.recorded(), accounts.calledWith())
			}
			accounts.err, lists.err = nil, nil
			resp = decode(t, serveRoot(srv, http.MethodPost, grantToken, "s1", callAccount))
			if resp.Error != nil || len(accounts.calledWith()) != 1 {
				t.Errorf("retry = %+v, account calls %v", resp, accounts.calledWith())
			}
		})
	}
}

func TestRootCallOfAShadowedAccountToolGoesToTheBox(t *testing.T) {
	srv, _, agents, lists := newUnifiedServer(t)
	accounts := newFakeConnectors()
	accounts.owned["shell"] = true
	srv.EnableConnectors(accounts)
	srv.EnableGrants(fakeGrants{boxes: []string{boxes.BoxID("alice")}, allAccounts: true})
	shadowed := strings.Replace(callShell, `"shell"`, `"shell_x"`, 1)
	lists.put(latestImage, users.ToolList{Image: latestImage, Tools: jsontext.Value(`[{"name":"shell_x"}]`)})

	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, grantToken, "s1", shadowed)).Result); text != shellAnswer+boxes.BoxID("alice") {
		t.Fatalf("shadowed call = %q, want the box's answer", text)
	}
	if len(accounts.calledWith()) != 0 || len(agents.calledOn()) != 1 {
		t.Errorf("Connect calls %v, box calls %v", accounts.calledWith(), agents.calledOn())
	}
}

func TestAGrantWithAccountsWhoseBoxesFailToReadStillAnnouncesListChanged(t *testing.T) {
	srv, _, _, _ := newUnifiedServer(t)
	srv.EnableConnectors(newFakeConnectors())
	srv.EnableGrants(fakeGrants{allAccounts: true, boxesErr: errors.New("db down")})
	caps, _ := decode(t, serveRoot(srv, http.MethodPost, grantToken, "", initialize)).Result["capabilities"].(map[string]any)
	if tools, _ := caps["tools"].(map[string]any); tools["listChanged"] != true {
		t.Errorf("a grant with accounts whose boxes fail to read announces %v", caps)
	}
}

func TestRootStreamsListChangedToAGrantOfAllAccounts(t *testing.T) {
	srv, _, _, _ := newUnifiedServer(t)
	accounts := newFakeConnectors()
	srv.EnableConnectors(accounts)
	srv.EnableGrants(fakeGrants{})
	if rec := serveRoot(srv, http.MethodGet, grantToken, "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET of a grant without accounts = %d, want 405", rec.Code)
	}
	caps, _ := decode(t, serveRoot(srv, http.MethodPost, grantToken, "", initialize)).Result["capabilities"].(map[string]any)
	if tools, _ := caps["tools"].(map[string]any); tools["listChanged"] != nil {
		t.Errorf("a grant without accounts announces %v", caps)
	}

	srv.EnableGrants(fakeGrants{allAccounts: true})
	caps, _ = decode(t, serveRoot(srv, http.MethodPost, grantToken, "", initialize)).Result["capabilities"].(map[string]any)
	if tools, _ := caps["tools"].(map[string]any); tools["listChanged"] != true {
		t.Errorf("a grant of all accounts announces %v", caps)
	}
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, RootPath, nil)
	req.Header.Set("Authorization", "Bearer "+grantToken)
	req.Header.Set("Accept", mcpwire.EventStream)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(rec, req)
	}()
	accounts.changed <- struct{}{}
	cancel()
	<-done
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != mcpwire.EventStream || !strings.Contains(rec.Body.String(), "notifications/tools/list_changed") {
		t.Errorf("stream = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	if got := accounts.watched(); !slices.Equal(got, []string{"alice"}) {
		t.Errorf("watched %v", got)
	}
}

func TestRootCallsOnTwoApiProcessesReachOneAgentSession(t *testing.T) {
	held := clustertest.Open(t)
	srv, fake, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	srv.EnableShared(nil, held)
	peer := newPeerServer(srv, fake, agents, lists, held)
	grants := fakeGrants{boxes: []string{boxes.BoxID("alice")}}
	srv.EnableGrants(grants)
	peer.EnableGrants(grants)

	session := serveRoot(srv, http.MethodPost, grantToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, grantToken, session, navigate)).Result); text != "opened https://example.com" {
		t.Fatalf("navigate on the first process = %q", text)
	}
	if text := callText(t, decode(t, serveRoot(peer, http.MethodPost, grantToken, session, snapshot)).Result); text != "page https://example.com" {
		t.Fatalf("a grant's snapshot on the other process = %q, want the page the navigate opened", text)
	}

	mine := serveRoot(peer, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	decode(t, serveRoot(peer, http.MethodPost, aliceToken, mine, navigate))
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, aliceToken, mine, snapshot)).Result); text != "page https://example.com" {
		t.Fatalf("a user token's snapshot on the other process = %q", text)
	}
	if n := len(slices.DeleteFunc(agents.recorded(), func(m string) bool { return m != "initialize" })); n != 2 {
		t.Fatalf("agent saw %v, want one initialize per client", agents.recorded())
	}

	third := newPeerServer(srv, fake, agents, lists, held)
	if rec := serveRoot(third, http.MethodDelete, aliceToken, mine, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE on a process that never saw the session = %d", rec.Code)
	}
	if got := agents.recorded(); got[len(got)-1] != http.MethodDelete {
		t.Fatalf("agent saw %v, want the other process's agent session closed", got)
	}
	if up, err := held.AgentSession(t.Context(), sessionClient("alice", mine), boxes.BoxID("alice")); err != nil || up != "" {
		t.Errorf("record after DELETE = %q, %v, want none", up, err)
	}
}

func TestTwoApiProcessesOpeningAtOnceKeepTheFirstRecordedAgentSession(t *testing.T) {
	held := clustertest.Open(t)
	srv, fake, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	srv.EnableShared(nil, held)
	peer := newPeerServer(srv, fake, agents, lists, &missOnce{AgentSessionStore: held})
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, navigate))

	if text := callText(t, decode(t, serveRoot(peer, http.MethodPost, aliceToken, session, snapshot)).Result); text != "page https://example.com" {
		t.Fatalf("the process that lost the race read %q, want the recorded session's page", text)
	}
	if want := []string{"initialize", "tools/call", "initialize", "DELETE", "tools/call"}; !slices.Equal(agents.recorded(), want) {
		t.Fatalf("agent saw %v, want the losing process to close its own session, %v", agents.recorded(), want)
	}
}

func TestAnAgentSessionGoneOnOneApiProcessMovesBothToItsSuccessor(t *testing.T) {
	held := clustertest.Open(t)
	srv, fake, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	srv.EnableShared(nil, held)
	peer := newPeerServer(srv, fake, agents, lists, held)
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell))
	decode(t, serveRoot(peer, http.MethodPost, aliceToken, session, callShell))

	agents.forget()
	decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, navigate))
	if text := callText(t, decode(t, serveRoot(peer, http.MethodPost, aliceToken, session, snapshot)).Result); text != "page https://example.com" {
		t.Fatalf("the other process after the session went = %q, want the successor's page", text)
	}
	if n := len(slices.DeleteFunc(agents.recorded(), func(m string) bool { return m != "initialize" })); n != 2 {
		t.Fatalf("agent saw %v, want one successor in all", agents.recorded())
	}
}

func TestAWakeOnOneApiProcessKeepsBothOnTheSessionThatOutlivedTheSleep(t *testing.T) {
	held := clustertest.Open(t)
	srv, fake, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	srv.EnableShared(nil, held)
	peer := newPeerServer(srv, fake, agents, lists, held)
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell))
	decode(t, serveRoot(peer, http.MethodPost, aliceToken, session, callShell))

	fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, navigate)).Result); text != "opened https://example.com" {
		t.Fatalf("the call that woke the box = %q", text)
	}
	if text := callText(t, decode(t, serveRoot(peer, http.MethodPost, aliceToken, session, snapshot)).Result); text != "page https://example.com" {
		t.Fatalf("the other process after the wake = %q, want the page the woken session opened", text)
	}
	if calls := agents.calledOn(); len(slices.Compact(calls)) != 1 {
		t.Errorf("calls went to agent sessions %v, want the one that outlived the sleep", calls)
	}
	if got := agents.recorded(); !slices.Contains(got, http.MethodDelete) {
		t.Errorf("agent saw %v, want the probe session the wake opened closed", got)
	}
}

func TestAColdWakeOnOneApiProcessMovesBothToANewSession(t *testing.T) {
	held := clustertest.Open(t)
	srv, fake, agents, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	srv.EnableShared(nil, held)
	peer := newPeerServer(srv, fake, agents, lists, held)
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell))
	decode(t, serveRoot(peer, http.MethodPost, aliceToken, session, callShell))

	fake.setState("alice", controlv1.BoxState_BOX_STATE_HIBERNATED)
	agents.forget()
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, navigate)).Result); text != "opened https://example.com" {
		t.Fatalf("the call that woke the box anew = %q", text)
	}
	if text := callText(t, decode(t, serveRoot(peer, http.MethodPost, aliceToken, session, snapshot)).Result); text != "page https://example.com" {
		t.Fatalf("the other process after a cold wake = %q, want the new session's page", text)
	}
}

func TestAFailedRecordReadHidesTheStoreAndARereadFailureKeepsTheHeldSession(t *testing.T) {
	held := &flakyStore{AgentSessionStore: clustertest.Open(t), fail: errors.New("failed to connect to `user=cocoonbox database=cluster`: 10.1.2.3:5432")}
	srv, _, _, lists := newUnifiedServer(t)
	lists.put("img:1", users.ToolList{Image: "img:1", Tools: jsontext.Value(boxTools)})
	srv.EnableShared(nil, held)
	session := serveRoot(srv, http.MethodPost, aliceToken, "", initialize).Header().Get(mcpwire.SessionHeader)
	if got := refusal(t, decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell))); got != status.Convert(errSessionStore).Message() {
		t.Fatalf("a call whose record read failed = %q, want the store hidden", got)
	}

	held.set(nil)
	decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell))
	held.set(errors.New("connection reset"))
	srv.mu.Lock()
	sess := srv.sessions["alice\n"+session]
	srv.mu.Unlock()
	sess.agents.mu.Lock()
	sess.agents.read[boxes.BoxID("alice")] = time.Now().Add(-2 * rereadEvery)
	sess.agents.mu.Unlock()
	if text := callText(t, decode(t, serveRoot(srv, http.MethodPost, aliceToken, session, callShell)).Result); text != shellAnswer+boxes.BoxID("alice") {
		t.Fatalf("a call whose hourly reread failed = %q, want the held session used", text)
	}
}

type fakeGrants struct {
	boxes       []string
	chosen      []string
	allAccounts bool
	boxesErr    error
}

func (g fakeGrants) Authenticate(context.Context, string) (oauth.Grant, error) {
	return oauth.Grant{
		ID: "g1", User: "alice", ClientName: "Agent", MaxBoxes: 2, MaxRunning: 1, Boxes: g.boxes, Chosen: g.chosen, AllAccounts: g.allAccounts,
	}, nil
}

func (g fakeGrants) Boxes(context.Context, string) ([]string, error) { return g.boxes, g.boxesErr }

func (fakeGrants) AddBox(context.Context, string, string) error { return nil }

func (fakeGrants) Challenge(bool) string { return "Bearer" }

type fakeAgents struct {
	boxes *fakeBoxes

	mu       sync.Mutex
	sent     []string
	posted   []string
	called   []string
	sessions map[string]bool
	pages    map[string]string
	deaf     bool
	lost     bool
	hold     *sync.WaitGroup
	oversize string
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
		ID     jsontext.Value `json:"id"`
		Method string         `json:"method"`
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
	if msg.Method == a.oversize {
		if msg.Method == "initialize" {
			session = "up" + string(rune('a'+len(a.sent)))
			a.sessions[session] = true
		}
		return use(answer(http.StatusOK, session, `{"jsonrpc":"2.0","id":`+cmp.Or(string(msg.ID), "0")+`,"result":{"pad":"`+strings.Repeat("x", maxAnswer)+`"}}`))
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
		text := shellAnswer + box
		var call struct {
			Params callParams `json:"params"`
		}
		_ = json.Unmarshal(body, &call)
		switch call.Params.Name {
		case "browser_navigate":
			var args struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(call.Params.Arguments, &args)
			a.pages[session], text = args.URL, "opened "+args.URL
		case "browser_snapshot":
			text = "page " + a.pages[session]
		}
		return use(answer(http.StatusOK, "", `{"jsonrpc":"2.0","id":`+string(msg.ID)+`,"result":{"content":[{"type":"text","text":"`+text+`"}]}}`))
	}
}

func (a *fakeAgents) set(fn func(*fakeAgents)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(a)
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

// missOnce reads no record once, as a process that looked before another recorded its session.
type missOnce struct {
	AgentSessionStore
	missed bool
}

func (m *missOnce) AgentSession(ctx context.Context, client, box string) (string, error) {
	if !m.missed {
		m.missed = true
		return "", nil
	}
	return m.AgentSessionStore.AgentSession(ctx, client, box)
}

type flakyStore struct {
	AgentSessionStore

	mu   sync.Mutex
	fail error
}

func (f *flakyStore) AgentSession(ctx context.Context, client, box string) (string, error) {
	if err := f.failure(); err != nil {
		return "", err
	}
	return f.AgentSessionStore.AgentSession(ctx, client, box)
}

func (f *flakyStore) PutAgentSession(ctx context.Context, client, box, upstream string) (string, error) {
	if err := f.failure(); err != nil {
		return "", err
	}
	return f.AgentSessionStore.PutAgentSession(ctx, client, box, upstream)
}

func (f *flakyStore) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

func (f *flakyStore) failure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail
}

type fakeConnectors struct {
	changed chan struct{}

	mu      sync.Mutex
	owned   map[string]bool
	lists   []string
	calls   []string
	watches []string
	err     error
}

func newFakeConnectors() *fakeConnectors {
	return &fakeConnectors{changed: make(chan struct{}), owned: map[string]bool{"mock": true}}
}

func (f *fakeConnectors) Reaches(_ context.Context, _ string, accounts bool, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	connector, _, _ := strings.Cut(name, "_")
	return accounts && f.owned[connector], f.err
}

func (f *fakeConnectors) GrantedTools(_ context.Context, user string, accounts bool) ([]map[string]jsontext.Value, error) {
	f.mu.Lock()
	f.lists = append(f.lists, fmt.Sprintf("%s %t", user, accounts))
	f.mu.Unlock()
	var tools []map[string]jsontext.Value
	err := json.Unmarshal([]byte(accountTools), &tools)
	return tools, err
}

func (f *fakeConnectors) CallGranted(_ context.Context, user, client string, accounts bool, name string, args jsontext.Value) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s %s %t %s %s", user, client, accounts, name, args))
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": name + " done"}}}, nil
}

func (f *fakeConnectors) ServeChanges(w http.ResponseWriter, r *http.Request, user string) {
	f.mu.Lock()
	f.watches = append(f.watches, user)
	f.mu.Unlock()
	w.Header().Set("Content-Type", mcpwire.EventStream)
	w.WriteHeader(http.StatusOK)
	select {
	case <-f.changed:
		_, _ = w.Write([]byte("notifications/tools/list_changed"))
	case <-r.Context().Done():
	}
}

func (f *fakeConnectors) listed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lists)
}

func (f *fakeConnectors) calledWith() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeConnectors) watched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.watches)
}

type fakeLists struct {
	mu    sync.Mutex
	lists map[string]users.ToolList
	err   error
}

func (f *fakeLists) ToolList(_ context.Context, image, family string) (users.ToolList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return users.ToolList{}, f.err
	}
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
	agents := &fakeAgents{boxes: fake, sessions: map[string]bool{}, pages: map[string]string{}}
	lists := &fakeLists{lists: map[string]users.ToolList{}}
	srv.EnableBoxTools(agents, lists)
	return srv, fake, agents, lists
}

func newPeerServer(srv *Server, fake *fakeBoxes, agents *fakeAgents, lists *fakeLists, held AgentSessionStore) *Server {
	peer := New(fake, srv.authn)
	peer.EnableBoxTools(agents, lists)
	peer.EnableShared(nil, held)
	return peer
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

func callText(t *testing.T, result any) string {
	t.Helper()
	r, _ := result.(map[string]any)
	content, _ := r["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("not a tool result: %v", result)
	}
	return content[0].(map[string]any)["text"].(string)
}

func listedNames(tools []any) (names []string) {
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}
