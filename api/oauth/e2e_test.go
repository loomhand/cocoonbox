package oauth_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/account"
	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/boxes"
	"github.com/loomhand/cocoonbox/api/console"
	"github.com/loomhand/cocoonbox/api/controlmcp"
	"github.com/loomhand/cocoonbox/api/edge"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/gwkeys"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/oauth"
	"github.com/loomhand/cocoonbox/api/users"
)

const (
	alicePassword = "alice-password"
	callback      = "http://127.0.0.1/callback"
)

func TestAgentGetsAGrantThroughConsentAndActsOnlyOnItsOwnBoxes(t *testing.T) {
	e := newEnv(t)
	resp := e.mcp(t, "", "tools/list", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(challenge, `resource_metadata="`+e.srv.URL+`/.well-known/oauth-protected-resource/cocoonbox.mcp"`) || !strings.Contains(challenge, `scope="boxes"`) {
		t.Fatalf("challenge = %q", challenge)
	}
	var prm map[string]any
	e.getJSON(t, "/.well-known/oauth-protected-resource/cocoonbox.mcp/control", &prm)
	if prm["resource"] != e.resource || e.resource != e.srv.URL+"/cocoonbox.mcp" {
		t.Errorf("protected resource = %v", prm["resource"])
	}
	var asm map[string]any
	e.getJSON(t, oauth.ServerMetadataPath, &asm)
	if asm["issuer"] != e.srv.URL || asm["client_id_metadata_document_supported"] != true || fmt.Sprint(asm["code_challenge_methods_supported"]) != "[S256]" {
		t.Errorf("server metadata = %v", asm)
	}

	clientID := e.register(t, "Test Agent")
	access, refresh := e.authorize(t, clientID, oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 7})

	if list := e.boxNames(t, access); len(list) != 0 {
		t.Fatalf("a fresh grant sees %v, want none of the user's boxes", list)
	}
	if text := e.toolError(t, access, "box_ensure", map[string]any{}); !strings.Contains(text, "created no box") {
		t.Errorf("ensure without a box: %q", text)
	}
	if text := e.toolError(t, access, "box_status", map[string]any{"box": "main"}); !strings.Contains(text, `no box "main"`) {
		t.Errorf("status of the user's own box: %q", text)
	}
	if text := e.toolError(t, access, "box_delete", map[string]any{"box": "main", "confirm": true}); !strings.Contains(text, `no box "main"`) {
		t.Errorf("delete of the user's own box: %q", text)
	}
	if u, _ := e.users.Get(e.alice); u.MaxBoxes != 1 {
		t.Fatalf("alice may have %d boxes, want the default 1, which main uses", u.MaxBoxes)
	}
	e.call(t, access, "box_create", map[string]any{"name": "agent-one"})
	if text := e.toolError(t, access, "box_create", map[string]any{"name": "agent-two"}); !strings.Contains(text, "keep 1 box at most") {
		t.Errorf("create past max boxes: %q", text)
	}
	if list := e.boxNames(t, access); !slices.Equal(list, []string{"agent-one"}) {
		t.Errorf("grant sees %v, want its own box", list)
	}
	if list := e.boxNames(t, e.userToken); !slices.Equal(list, []string{"main", "agent-one"}) {
		t.Errorf("the user token sees %v, want every box", list)
	}
	if _, err := e.authn.Authenticate(t.Context(), "Bearer "+access); err == nil {
		t.Error("the user authenticator admits an access token, so gRPC, the box MCP and Connect would take it")
	}

	newAccess, newRefresh := e.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if newRefresh == refresh || newAccess == access {
		t.Fatal("refresh did not rotate")
	}
	if list := e.boxNames(t, newAccess); !slices.Equal(list, []string{"agent-one"}) {
		t.Errorf("after refresh the grant sees %v", list)
	}
	grants := e.grants(t)
	life := lifetime(e.users, e.alice, grants[0].ID)
	if code, body := e.tokenError(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); code != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") || life.Err() == nil {
		t.Fatalf("replayed refresh token: %d %s; the grant's calls' lifetime %v", code, body, life.Err())
	}
	if resp := e.mcp(t, newAccess, "tools/list", nil); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "invalid_token") {
		t.Errorf("after a replay the grant still works: %d", resp.StatusCode)
	}
	if list := e.boxNames(t, e.userToken); !slices.Equal(list, []string{"main", "agent-one"}) {
		t.Errorf("a revoked grant's box left the user: %v", list)
	}
}

func TestConsoleListsAcknowledgesAndRevokesGrants(t *testing.T) {
	e := newEnv(t)
	clientID := e.register(t, "Listed Agent")
	access, _ := e.authorize(t, clientID, oauth.Caps{MaxBoxes: 2, MaxRunning: 1, Days: 30})
	e.call(t, access, "box_create", map[string]any{"name": "made"})

	grants := e.grants(t)
	if len(grants) != 1 || grants[0].ClientName != "Listed Agent" || grants[0].Seen || len(grants[0].Boxes) != 1 || grants[0].ClientHost != "" {
		t.Fatalf("grants = %+v", grants)
	}
	if grants[0].ExpiresAt.Sub(grants[0].CreatedAt).Round(time.Hour) != 30*24*time.Hour {
		t.Errorf("grant lasts %s, want 30 days", grants[0].ExpiresAt.Sub(grants[0].CreatedAt))
	}
	e.console(t, http.MethodPost, "agents/seen", map[string]any{}, nil)
	grants = e.grants(t)
	if !grants[0].Seen {
		t.Error("seen did not stick")
	}
	life := lifetime(e.users, e.alice, grants[0].ID)
	e.console(t, http.MethodDelete, "agents/"+grants[0].ID, map[string]any{}, nil)
	if resp := e.mcp(t, access, "tools/list", nil); resp.StatusCode != http.StatusUnauthorized || life.Err() == nil {
		t.Errorf("a revoked grant answers %d, want 401; its calls' lifetime %v", resp.StatusCode, life.Err())
	}
}

func TestTokenEndpointRefusesABadRedemption(t *testing.T) {
	e := newEnv(t)
	clientID := e.register(t, "Agent")
	other := e.register(t, "Other")
	tests := []struct {
		name   string
		mutate func(v url.Values)
	}{
		{"wrong verifier", func(v url.Values) { v.Set("code_verifier", strings.Repeat("x", 43)) }},
		{"another client", func(v url.Values) { v.Set("client_id", other) }},
		{"another redirect", func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:9/other") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, verifier, redirect := e.code(t, clientID, oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 1})
			v := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
			bad := maps.Clone(v)
			tt.mutate(bad)
			if status, body := e.tokenError(t, bad); status != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
				t.Errorf("%d %s, want invalid_grant", status, body)
			}
			if status, body := e.tokenError(t, v); status != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
				t.Errorf("the right redemption after a refused one: %d %s, want invalid_grant", status, body)
			}
		})
	}
	code, verifier, redirect := e.code(t, clientID, oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 1})
	v := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
	e.token(t, v)
	if status, body := e.tokenError(t, v); status != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
		t.Errorf("a code redeemed twice: %d %s", status, body)
	}
}

func TestAuthorizeRefusesUntrustedRequests(t *testing.T) {
	e := newEnv(t)
	clientID := e.register(t, "Agent")
	base := func() url.Values {
		return url.Values{
			"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {callback}, "code_challenge": {challenge(strings.Repeat("v", 43))},
			"code_challenge_method": {"S256"}, "state": {"s1"}, "scope": {"boxes"}, "resource": {e.resource},
		}
	}
	tests := []struct {
		name     string
		mutate   func(q url.Values)
		redirect bool
	}{
		{"forged client id", func(q url.Values) { q.Set("client_id", clientID+"x") }, false},
		{"unregistered redirect", func(q url.Values) { q.Set("redirect_uri", "https://evil.example/cb") }, false},
		{"plain PKCE", func(q url.Values) { q.Set("code_challenge_method", "plain") }, true},
		{"another scope", func(q url.Values) { q.Set("scope", "boxes keys") }, true},
		{"another resource", func(q url.Values) { q.Set("resource", e.srv.URL+"/cocoonbox.mcpx") }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := base()
			tt.mutate(q)
			status, body := e.consoleStatus(t, http.MethodPost, "oauth/check", map[string]any{"query": q.Encode()})
			var refusal struct {
				Redirect string `json:"redirect"`
			}
			_ = json.Unmarshal([]byte(body), &refusal)
			if status != http.StatusBadRequest || (refusal.Redirect != "") != tt.redirect {
				t.Errorf("%d %s, want 400 with redirect %t", status, body, tt.redirect)
			}
			if tt.redirect && (!strings.Contains(refusal.Redirect, "state=s1") || !strings.HasPrefix(refusal.Redirect, "http://127.0.0.1/callback?")) {
				t.Errorf("redirect = %q", refusal.Redirect)
			}
		})
	}
}

func TestDenyAndRevocationEndTheFlow(t *testing.T) {
	e := newEnv(t)
	clientID := e.register(t, "Agent")
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {callback}, "code_challenge": {challenge(strings.Repeat("v", 43))},
		"code_challenge_method": {"S256"}, "state": {"s2"}, "scope": {"boxes"},
	}
	var denied struct {
		Redirect string `json:"redirect"`
	}
	e.console(t, http.MethodPost, "oauth/deny", map[string]any{"query": q.Encode()}, &denied)
	if u, err := url.Parse(denied.Redirect); err != nil || u.Query().Get("error") != "access_denied" || u.Query().Get("state") != "s2" || u.Query().Get("code") != "" {
		t.Errorf("deny redirect = %q", denied.Redirect)
	}
	access, refresh := e.authorize(t, clientID, oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 1})
	grants := e.grants(t)
	life := lifetime(e.users, e.alice, grants[0].ID)
	if resp := e.post(t, oauth.RevokePath, "application/x-www-form-urlencoded", url.Values{"token": {refresh}, "client_id": {clientID}}.Encode()); resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if resp := e.mcp(t, access, "tools/list", nil); resp.StatusCode != http.StatusUnauthorized || life.Err() == nil {
		t.Errorf("after revocation the access token answers %d; its calls' lifetime %v", resp.StatusCode, life.Err())
	}
}

func TestSigningOutEverywhereEndsEverySignInAndKeepsTheBoxes(t *testing.T) {
	e := newEnv(t)
	first, _ := e.authorize(t, e.register(t, "First Agent"), oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 7})
	second, _ := e.authorize(t, e.register(t, "Second Agent"), oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 7})
	e.call(t, first, "box_create", map[string]any{"name": "agent-one"})
	device, err := e.users.IssueDevice(t.Context(), e.alice, oauth.CLIClient, "laptop")
	if err != nil {
		t.Fatalf("IssueDevice: %v", err)
	}
	life := lifetime(e.users, e.alice, "")
	if code, body := e.consoleStatus(t, http.MethodPost, "me/sign-out-everywhere", map[string]any{}); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere = %d %s", code, body)
	}
	for name, token := range map[string]string{"the first agent": first, "the second agent": second} {
		if resp := e.mcp(t, token, "tools/list", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s answers %d after signing out everywhere, want 401", name, resp.StatusCode)
		}
	}
	for name, token := range map[string]string{"the personal token": e.userToken, "the device token": device} {
		if _, err := e.authn.Authenticate(t.Context(), "Bearer "+token); err == nil {
			t.Errorf("%s still authenticates", name)
		}
	}
	if life.Err() == nil {
		t.Error("the user's open calls outlived signing out everywhere")
	}
	conn, err := pgx.Connect(t.Context(), e.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var sessions, devices, grants int
	var token *string
	err = conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sessions WHERE user_id = $1), (SELECT count(*) FROM user_tokens WHERE user_id = $1),
		(SELECT count(*) FROM oauth_grants WHERE user_id = $1), (SELECT token_sha256 FROM users WHERE id = $1)`, e.alice).Scan(&sessions, &devices, &grants, &token)
	if err != nil || sessions+devices+grants != 0 || token != nil {
		t.Errorf("after signing out everywhere: %d sessions, %d device tokens, %d grants, personal token %v, %v", sessions, devices, grants, token, err)
	}
	if !slices.ContainsFunc(e.users.Boxes(e.alice), func(b users.Box) bool { return b.Name == "agent-one" }) {
		t.Errorf("an agent's box left the user: %+v", e.users.Boxes(e.alice))
	}
}

func TestAGrantRevokedWhileItsBoxWakesEndsTheExchange(t *testing.T) {
	e := newEnv(t)
	access, _ := e.authorize(t, e.register(t, "Agent"), oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 1})
	var made struct {
		BoxID string `json:"box_id"`
	}
	if err := json.Unmarshal(e.call(t, access, "box_create", map[string]any{"name": "agent-one"}), &made); err != nil {
		t.Fatalf("box_create: %v", err)
	}
	grants := e.grants(t)
	e.waking.slow.Store(true)
	answered := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, e.srv.URL+edge.MCPPrefix+made.BoxID, nil)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Authorization", "Bearer "+access)
		resp, err := e.client.Do(req)
		if err != nil {
			answered <- 0
			return
		}
		_ = resp.Body.Close()
		answered <- resp.StatusCode
	}()
	<-e.waking.entered
	e.console(t, http.MethodDelete, "agents/"+grants[0].ID, map[string]any{}, nil)
	select {
	case code := <-answered:
		if code == http.StatusOK {
			t.Errorf("a grant revoked while its box woke got its stream: %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the exchange of a revoked grant waited out its box's wake")
	}
}

func TestAGrantDrivesOnlyItsOwnBoxesThroughTheBoxMCP(t *testing.T) {
	e := newEnv(t)
	access, _ := e.authorize(t, e.register(t, "Agent"), oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 1})
	var made struct {
		BoxID string `json:"box_id"`
	}
	if err := json.Unmarshal(e.call(t, access, "box_create", map[string]any{"name": "agent-one"}), &made); err != nil {
		t.Fatalf("box_create: %v", err)
	}
	main := e.users.Boxes(e.alice)[0].ID
	tests := []struct {
		name  string
		token string
		box   string
		want  int
	}{
		{"the grant on its box", access, made.BoxID, http.StatusOK},
		{"the grant on the user's box", access, main, http.StatusNotFound},
		{"the user on the user's box", e.userToken, main, http.StatusOK},
		{"a bad access token", oauth.AccessPrefix + "00", made.BoxID, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := e.boxMCP(t, tt.token, tt.box)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			switch tt.want {
			case http.StatusOK:
				if got := resp.Header.Get("X-Seen-Credential"); got != "cred:"+tt.box || resp.Header.Get("X-Seen-Authorization") != "" {
					t.Errorf("the agent saw credential %q and authorization %q", got, resp.Header.Get("X-Seen-Authorization"))
				}
			case http.StatusUnauthorized:
				if !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
					t.Errorf("challenge = %q", resp.Header.Get("WWW-Authenticate"))
				}
			}
		})
	}
	var prm map[string]any
	e.getJSON(t, "/.well-known/oauth-protected-resource/cocoonbox.mcp/"+made.BoxID, &prm)
	if prm["resource"] != e.resource {
		t.Errorf("box resource metadata = %v", prm)
	}
}

func TestAGrantReachesTheBoxesTheUserChoseAndNoOther(t *testing.T) {
	e := newEnv(t)
	two := 2
	if _, err := e.users.Update(t.Context(), e.alice, users.Settings{MaxBoxesCap: &two, MaxBoxes: &two}); err != nil {
		t.Fatalf("raise max boxes: %v", err)
	}
	work, err := e.users.AddBox(t.Context(), users.Box{ID: "cb-work00000000", Owner: e.alice, Name: "work"})
	if err != nil {
		t.Fatalf("add box: %v", err)
	}
	bob, err := e.users.Create(t.Context(), users.NewUser{Name: "bob", Password: alicePassword})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	main, bobs := e.users.Boxes(e.alice)[0].ID, e.users.Boxes(bob.ID)[0].ID
	access, _ := e.authorize(t, e.register(t, "Agent"), oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 1}, main, bobs)
	grant := e.grants(t)[0]
	if !slices.Equal(grant.Chosen, []string{main}) {
		t.Fatalf("chosen = %v, want only the user's own box %s", grant.Chosen, main)
	}

	if list := e.boxNames(t, access); !slices.Equal(list, []string{"main"}) {
		t.Errorf("the grant sees %v, want its chosen box", list)
	}
	if text := e.toolError(t, access, "box_status", map[string]any{"box": "work"}); !strings.Contains(text, `no box "work"`) {
		t.Errorf("status of an unchosen box: %q", text)
	}
	if text := e.toolError(t, access, "box_delete", map[string]any{"box": "main", "confirm": true}); text != "This client may delete only the boxes it created." {
		t.Errorf("delete of a chosen box: %q", text)
	}
	var ensured struct {
		BoxID string `json:"box_id"`
	}
	if err := json.Unmarshal(e.call(t, access, "box_ensure", map[string]any{}), &ensured); err != nil || ensured.BoxID != main {
		t.Errorf("ensure without a box woke %q, want the chosen box: %v", ensured.BoxID, err)
	}
	var made struct {
		BoxID string `json:"box_id"`
	}
	if err := json.Unmarshal(e.call(t, access, "box_create", map[string]any{"name": "agent-one"}), &made); err != nil {
		t.Fatalf("box_create past a chosen box on max boxes 1: %v", err)
	}
	if list := e.boxNames(t, access); !slices.Equal(list, []string{"main", "agent-one"}) {
		t.Errorf("the grant sees %v, want its chosen and its created box", list)
	}
	if i := slices.IndexFunc(e.users.Boxes(e.alice), func(b users.Box) bool { return b.ID == main }); e.users.Boxes(e.alice)[i].Agent {
		t.Error("a chosen box became an agent box, so it would lose its owner credentials and hibernate as one")
	}

	for _, tt := range []struct {
		name  string
		token string
		box   string
		want  int
	}{
		{"the grant on its chosen box", access, main, http.StatusOK},
		{"the grant on its created box", access, made.BoxID, http.StatusOK},
		{"the grant on an unchosen box", access, work.ID, http.StatusNotFound},
		{"the grant on another user's box", access, bobs, http.StatusNotFound},
		{"the user on an unchosen box", e.userToken, work.ID, http.StatusOK},
	} {
		if resp := e.boxMCP(t, tt.token, tt.box); resp.StatusCode != tt.want {
			t.Errorf("box MCP, %s: %d, want %d", tt.name, resp.StatusCode, tt.want)
		}
	}

	for _, tt := range []struct {
		name  string
		token string
		args  map[string]any
		fails bool
		want  string
	}{
		{"the grant's default box", access, map[string]any{"command": "id"}, false, "cred:" + main},
		{"the grant's created box", access, map[string]any{"command": "id", "box": "agent-one"}, false, "cred:" + made.BoxID},
		{"an unchosen box", access, map[string]any{"command": "id", "box": "work"}, true, `You have no box "work"; box_list lists your boxes.`},
		{"another user's box", access, map[string]any{"command": "id", "box": bobs}, true, `You have no box "` + bobs + `"; box_list lists your boxes.`},
		{"the user's named box", e.userToken, map[string]any{"command": "id", "box": "work"}, false, "cred:" + work.ID},
		{"the user's default box", e.userToken, map[string]any{"command": "id"}, false, "cred:" + main},
	} {
		fails, text := e.rootCall(t, tt.token, "s1", tt.args)
		if fails != tt.fails || !strings.HasPrefix(text, tt.want) {
			t.Errorf("unified, %s: %v %q, want %v %q", tt.name, fails, text, tt.fails, tt.want)
		}
		if !fails && strings.Contains(text, `"box"`) {
			t.Errorf("unified, %s: the agent got the box argument: %s", tt.name, text)
		}
	}

	e.console(t, http.MethodPut, "agents/"+grant.ID+"/boxes", map[string]any{"boxes": []string{work.ID}}, nil)
	if list := e.boxNames(t, access); !slices.Equal(list, []string{"work", "agent-one"}) {
		t.Errorf("after the change the grant sees %v", list)
	}
	if resp := e.boxMCP(t, access, main); resp.StatusCode != http.StatusNotFound {
		t.Errorf("box MCP of a box no longer chosen: %d", resp.StatusCode)
	}
	if fails, text := e.rootCall(t, access, "s1", map[string]any{"command": "id"}); fails || !strings.HasPrefix(text, "cred:"+work.ID) {
		t.Errorf("unified default after the change: %v %q", fails, text)
	}
	if code, _ := e.consoleStatus(t, http.MethodPut, "agents/oag_missing/boxes", map[string]any{"boxes": []string{}}); code != http.StatusNotFound {
		t.Errorf("choosing for an unknown grant: %d", code)
	}
}

func TestAGrantKeepsOneAgentSessionAcrossClientSessionsUntilItEnds(t *testing.T) {
	e := newEnv(t)
	main := e.users.Boxes(e.alice)[0].ID
	access, _ := e.authorize(t, e.register(t, "Agent"), oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 1}, main)
	for i := range 3 {
		if fails, text := e.rootCall(t, access, fmt.Sprint("grant-", i), map[string]any{"command": "id"}); fails || !strings.HasPrefix(text, "cred:"+main) {
			t.Fatalf("call %d in a new client session: %v %q", i, fails, text)
		}
	}
	if seen := e.seen.list(); !slices.Equal(seen, []string{"initialize"}) {
		t.Fatalf("three client sessions of a grant: the agent saw %v, want one initialize", seen)
	}
	e.console(t, http.MethodDelete, "agents/"+e.grants(t)[0].ID, map[string]any{}, nil)
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(e.seen.list(), "DELETE agent-session") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if seen := e.seen.list(); !slices.Equal(seen, []string{"initialize", "DELETE agent-session"}) {
		t.Fatalf("after the revoke the agent saw %v, want the shared session closed", seen)
	}
	for i := range 2 {
		e.rootCall(t, e.userToken, fmt.Sprint("user-", i), map[string]any{"command": "id"})
	}
	if n := len(slices.DeleteFunc(e.seen.list(), func(m string) bool { return m != "initialize" })); n != 3 {
		t.Fatalf("two client sessions of the user token opened %d agent sessions in all, want 1 + 2", n)
	}
}

func TestSigningInAgainShowsAndReplacesTheChosenBoxes(t *testing.T) {
	e := newEnv(t)
	clientID := e.register(t, "Agent")
	main := e.users.Boxes(e.alice)[0].ID
	e.authorize(t, clientID, oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 1}, main)
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {callback}, "code_challenge": {challenge(strings.Repeat("v", 43))},
		"code_challenge_method": {"S256"},
	}
	var consent struct {
		oauth.Consent
		Boxes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"boxes"`
	}
	e.console(t, http.MethodPost, "oauth/check", map[string]any{"query": q.Encode()}, &consent)
	if !consent.Existing || !slices.Equal(consent.Chosen, []string{main}) || len(consent.Boxes) != 1 || consent.Boxes[0].ID != main {
		t.Fatalf("consent = %+v, want the renewal to show the chosen box among the user's boxes", consent)
	}
	access, _ := e.authorize(t, clientID, oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 1})
	if grants := e.grants(t); len(grants) != 1 || len(grants[0].Chosen) != 0 {
		t.Fatalf("grants = %+v, want the renewal to drop the chosen box", grants)
	}
	if resp := e.boxMCP(t, access, main); resp.StatusCode != http.StatusNotFound {
		t.Errorf("box MCP of the dropped box: %d", resp.StatusCode)
	}
}

func TestSigningInAgainRenewsTheClientsGrant(t *testing.T) {
	e := newEnv(t)
	clientID := e.register(t, "Agent")
	first, _ := e.authorize(t, clientID, oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 1})
	e.call(t, first, "box_create", map[string]any{"name": "agent-one"})
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {callback}, "code_challenge": {challenge(strings.Repeat("v", 43))},
		"code_challenge_method": {"S256"},
	}
	var consent oauth.Consent
	e.console(t, http.MethodPost, "oauth/check", map[string]any{"query": q.Encode()}, &consent)
	if !consent.Existing {
		t.Error("the consent page does not say the client already has access")
	}
	second, _ := e.authorize(t, clientID, oauth.Caps{MaxBoxes: 2, MaxRunning: 1, Days: 7})
	if list := e.boxNames(t, second); !slices.Equal(list, []string{"agent-one"}) {
		t.Errorf("the second sign-in sees %v, want the grant's box", list)
	}
	if list := e.boxNames(t, first); !slices.Equal(list, []string{"agent-one"}) {
		t.Errorf("the first sign-in's token sees %v after the renewal", list)
	}
	grants := e.grants(t)
	if len(grants) != 1 || grants[0].MaxBoxes != 2 {
		t.Errorf("grants = %+v, want one renewed to 2 boxes", grants)
	}
}

func TestDisabledUserLosesItsGrants(t *testing.T) {
	e := newEnv(t)
	access, _ := e.authorize(t, e.register(t, "Agent"), oauth.Caps{MaxBoxes: 1, MaxRunning: 1, Days: 1})
	disabled := true
	if _, err := e.users.Update(t.Context(), e.alice, users.Settings{Disabled: &disabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if resp := e.mcp(t, access, "tools/list", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a disabled user's grant answers %d", resp.StatusCode)
	}
}

type env struct {
	dsn       string
	waking    *wakeGate
	seen      *agentLog
	srv       *httptest.Server
	client    *http.Client
	users     *users.Service
	authn     *auth.Authenticator
	resource  string
	alice     string
	userToken string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := freshDatabase(t)
	us, err := users.Open(t.Context(), users.Options{
		DSN: dsn, MasterKeyFile: masterKeyFile(t), GW: &fakeGW{}, Tenants: fakeTenants{},
		FirstBoxID: func(user string) string { return "cb-" + hex.EncodeToString([]byte(user))[:12] },
	})
	if err != nil {
		t.Fatalf("open users: %v", err)
	}
	t.Cleanup(us.Close)
	alice, err := us.Create(t.Context(), users.NewUser{Name: "alice", Password: alicePassword})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	userToken, err := us.RegenerateToken(t.Context(), alice.ID)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	resource := srv.URL + strings.TrimSuffix(edge.MCPPrefix, "/")
	grants, err := oauth.Open(t.Context(), oauth.Options{DSN: dsn, Issuer: srv.URL, Resource: resource, Accounts: us})
	if err != nil {
		t.Fatalf("open oauth: %v", err)
	}
	t.Cleanup(grants.Close)
	authn := auth.New(us, us)
	store := &fakeBoxes{users: us, states: map[string]controlv1.BoxState{}}
	control := controlmcp.New(store, authn)
	control.EnableGrants(grants)
	web := console.New(us, store, 0, 2)
	web.EnableAgents(grants)
	signIns := account.New(us)
	signIns.EnableGrants(grants)
	web.EnableAccount(signIns)
	seen := &agentLog{}
	agent := newFakeAgent(t, seen)
	waking := &wakeGate{entered: make(chan struct{}, 1)}
	boxMCP := edge.NewMCP(fakeResolver{users: us, addr: agent.Listener.Addr().String(), waking: waking}, authn, fakeCredentials{})
	boxMCP.EnableGrants(grants)
	control.EnableBoxTools(boxMCP, us)
	mux.Handle(edge.MCPPrefix, boxMCP)
	mux.Handle(controlmcp.Path, control)
	mux.Handle(controlmcp.RootPath, control)
	mux.Handle(console.Prefix, web)
	mux.HandleFunc("GET "+grants.ResourceMetadataPath(), grants.ServeResourceMetadata)
	mux.HandleFunc("GET "+grants.ResourceMetadataPath()+"/", grants.ServeResourceMetadata)
	mux.HandleFunc("GET "+oauth.ServerMetadataPath, grants.ServeServerMetadata)
	mux.HandleFunc("POST "+oauth.TokenPath, grants.ServeToken)
	mux.HandleFunc("POST "+oauth.RegisterPath, grants.ServeRegister)
	mux.HandleFunc("POST "+oauth.RevokePath, grants.ServeRevoke)

	client := srv.Client()
	client.Jar, _ = cookiejar.New(nil)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	e := &env{dsn: dsn, waking: waking, seen: seen, srv: srv, client: client, users: us, authn: authn, resource: resource, alice: alice.ID, userToken: userToken}
	e.console(t, http.MethodPost, "login", map[string]any{"name": "alice", "password": alicePassword}, nil)
	return e
}

func (e *env) register(t *testing.T, name string) string {
	t.Helper()
	resp := e.post(t, oauth.RegisterPath, "application/json", fmt.Sprintf(`{"client_name":%q,"redirect_uris":[%q],"token_endpoint_auth_method":"none"}`, name, callback))
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if resp.StatusCode != http.StatusCreated || json.UnmarshalRead(resp.Body, &reg) != nil || reg.ClientID == "" {
		t.Fatalf("register: %d", resp.StatusCode)
	}
	return reg.ClientID
}

// code runs the consent page's calls, choosing boxes, and returns the code, its verifier and the redirect URI on a port of the client's choice.
func (e *env) code(t *testing.T, clientID string, caps oauth.Caps, boxes ...string) (string, string, string) {
	t.Helper()
	verifier := rand.Text() + rand.Text()
	redirect := "http://127.0.0.1:43123/callback"
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "code_challenge": {challenge(verifier)},
		"code_challenge_method": {"S256"}, "state": {"st"}, "scope": {"boxes"}, "resource": {e.resource},
	}
	var consent oauth.Consent
	e.console(t, http.MethodPost, "oauth/check", map[string]any{"query": q.Encode()}, &consent)
	if consent.ClientID != clientID || !consent.Loopback || consent.RedirectHost != "127.0.0.1:43123" {
		t.Fatalf("consent = %+v", consent)
	}
	var approved struct {
		Redirect string `json:"redirect"`
	}
	e.console(t, http.MethodPost, "oauth/approve", map[string]any{"query": q.Encode(), "max_boxes": caps.MaxBoxes, "max_running": caps.MaxRunning, "days": caps.Days, "boxes": boxes}, &approved)
	u, err := url.Parse(approved.Redirect)
	if err != nil || u.Query().Get("state") != "st" || u.Query().Get("iss") != e.srv.URL || u.Query().Get("code") == "" {
		t.Fatalf("redirect = %q", approved.Redirect)
	}
	return u.Query().Get("code"), verifier, redirect
}

func (e *env) authorize(t *testing.T, clientID string, caps oauth.Caps, boxes ...string) (string, string) {
	t.Helper()
	code, verifier, redirect := e.code(t, clientID, caps, boxes...)
	return e.token(t, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {redirect},
		"code_verifier": {verifier}, "resource": {e.resource},
	})
}

func (e *env) token(t *testing.T, form url.Values) (string, string) {
	t.Helper()
	resp := e.post(t, oauth.TokenPath, "application/x-www-form-urlencoded", form.Encode())
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
	}
	if resp.StatusCode != http.StatusOK || json.UnmarshalRead(resp.Body, &tok) != nil || tok.TokenType != "Bearer" || !strings.HasPrefix(tok.AccessToken, oauth.AccessPrefix) {
		t.Fatalf("token: %d", resp.StatusCode)
	}
	return tok.AccessToken, tok.RefreshToken
}

func (e *env) tokenError(t *testing.T, form url.Values) (int, string) {
	t.Helper()
	resp := e.post(t, oauth.TokenPath, "application/x-www-form-urlencoded", form.Encode())
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (e *env) mcp(t *testing.T, token, method string, params any) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv.URL+controlmcp.Path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("mcp %s: %v", method, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// result calls tool as token and returns the tool result.
func (e *env) result(t *testing.T, token, tool string, args map[string]any) (bool, string, jsontext.Value) {
	t.Helper()
	resp := e.mcp(t, token, "tools/call", map[string]any{"name": tool, "arguments": args})
	var out struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured jsontext.Value `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if resp.StatusCode != http.StatusOK || json.UnmarshalRead(resp.Body, &out) != nil || out.Error != nil || len(out.Result.Content) == 0 {
		t.Fatalf("%s: %d %+v", tool, resp.StatusCode, out.Error)
	}
	return out.Result.IsError, out.Result.Content[0].Text, out.Result.Structured
}

func (e *env) call(t *testing.T, token, tool string, args map[string]any) jsontext.Value {
	t.Helper()
	isError, text, structured := e.result(t, token, tool, args)
	if isError {
		t.Fatalf("%s: %s", tool, text)
	}
	return structured
}

func (e *env) toolError(t *testing.T, token, tool string, args map[string]any) string {
	t.Helper()
	isError, text, _ := e.result(t, token, tool, args)
	if !isError {
		t.Fatalf("%s succeeded: %s", tool, text)
	}
	return text
}

func (e *env) boxNames(t *testing.T, token string) []string {
	t.Helper()
	var list struct {
		Boxes []struct {
			Name string `json:"name"`
		} `json:"boxes"`
	}
	if err := json.Unmarshal(e.call(t, token, "box_list", map[string]any{}), &list); err != nil {
		t.Fatalf("box_list: %v", err)
	}
	var names []string
	for _, b := range list.Boxes {
		names = append(names, b.Name)
	}
	return names
}

func (e *env) grants(t *testing.T) []oauth.Grant {
	t.Helper()
	var agents struct {
		Grants []oauth.Grant `json:"grants"`
	}
	e.console(t, http.MethodGet, "agents", nil, &agents)
	return agents.Grants
}

// rootCall calls a box tool on the unified MCP as token in client session session and returns whether it failed and its text.
func (e *env) rootCall(t *testing.T, token, session string, args map[string]any) (bool, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "shell", "arguments": args}})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv.URL+controlmcp.RootPath, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", session)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("unified call: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.UnmarshalRead(resp.Body, &out); err != nil || len(out.Result.Content) == 0 {
		t.Fatalf("unified call: %d %v", resp.StatusCode, err)
	}
	return out.Result.IsError, out.Result.Content[0].Text
}

func (e *env) boxMCP(t *testing.T, token, box string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv.URL+edge.MCPPrefix+box, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("box mcp: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (e *env) console(t *testing.T, method, path string, body, out any) {
	t.Helper()
	status, data := e.consoleStatus(t, method, path, body)
	if status >= http.StatusMultipleChoices {
		t.Fatalf("%s %s: %d %s", method, path, status, data)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(data), out); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
}

func (e *env) consoleStatus(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = strings.NewReader(string(data))
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, e.srv.URL+console.Prefix+"api/"+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func (e *env) getJSON(t *testing.T, path string, out any) {
	t.Helper()
	resp, err := e.client.Get(e.srv.URL + path)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %v %v", path, err, resp)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.UnmarshalRead(resp.Body, out); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

func (e *env) post(t *testing.T, path, contentType, body string) *http.Response {
	t.Helper()
	resp, err := e.client.Post(e.srv.URL+path, contentType, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// fakeBoxes keeps box rows in the users service, as boxes.Service does, and every box runs at once.
type fakeBoxes struct {
	controlv1.UnimplementedBoxesServer
	users *users.Service

	mu     sync.Mutex
	states map[string]controlv1.BoxState
	ops    int
}

func (f *fakeBoxes) List(ctx context.Context, _ *controlv1.ListBoxesRequest) (*controlv1.ListBoxesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &controlv1.ListBoxesResponse{}
	for _, b := range f.users.Boxes(auth.User(ctx)) {
		resp.Boxes = append(resp.Boxes, &controlv1.Box{Id: b.ID, Name: b.Name, State: f.states[b.ID]})
	}
	return resp, nil
}

func (f *fakeBoxes) Create(ctx context.Context, req *controlv1.CreateBoxRequest) (*controlv1.EnsureResponse, error) {
	record := boxes.AgentRecord(ctx)
	b, err := f.users.AddBox(ctx, users.Box{ID: "cb-" + hex.EncodeToString([]byte(rand.Text()))[:12], Owner: auth.User(ctx), Name: req.GetName(), Agent: record != nil})
	if err != nil {
		return nil, err
	}
	if record != nil {
		if err := record(ctx, b.ID); err != nil {
			return nil, err
		}
	}
	return f.start(b.ID, b.Name), nil
}

func (f *fakeBoxes) Ensure(ctx context.Context, req *controlv1.EnsureRequest) (*controlv1.EnsureResponse, error) {
	list := f.users.Boxes(auth.User(ctx))
	i := slices.IndexFunc(list, func(b users.Box) bool { return req.GetBoxId() == "" || b.ID == req.GetBoxId() })
	if i < 0 {
		return nil, fmt.Errorf("no box %s", req.GetBoxId())
	}
	return f.start(list[i].ID, list[i].Name), nil
}

func (f *fakeBoxes) Hibernate(_ context.Context, ref *controlv1.BoxRef) (*controlv1.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[ref.GetBoxId()] = controlv1.BoxState_BOX_STATE_HIBERNATED
	f.ops++
	return &controlv1.Operation{Id: fmt.Sprint("op-", f.ops), BoxId: ref.GetBoxId()}, nil
}

func (f *fakeBoxes) Delete(ctx context.Context, ref *controlv1.BoxRef) (*controlv1.Operation, error) {
	if err := f.users.RemoveBox(ctx, auth.User(ctx), ref.GetBoxId()); err != nil {
		return nil, err
	}
	return &controlv1.Operation{Id: "op-delete", BoxId: ref.GetBoxId()}, nil
}

func (f *fakeBoxes) Rename(context.Context, *controlv1.RenameBoxRequest) (*controlv1.Box, error) {
	return nil, fmt.Errorf("rename is not faked")
}

func (f *fakeBoxes) WatchOperation(_ *controlv1.WatchOperationRequest, stream controlv1.Boxes_WatchOperationServer) error {
	return stream.Send(&controlv1.OperationEvent{Phase: controlv1.Phase_PHASE_DONE, Detail: "done"})
}

func (f *fakeBoxes) Release(context.Context, string) error { return nil }

func (f *fakeBoxes) HibernateAll(context.Context, string) error { return nil }

func (f *fakeBoxes) DeleteState(context.Context, string) error { return nil }

func (f *fakeBoxes) start(id, name string) *controlv1.EnsureResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[id] = controlv1.BoxState_BOX_STATE_RUNNING
	f.ops++
	return &controlv1.EnsureResponse{
		Box:       &controlv1.Box{Id: id, Name: name, State: controlv1.BoxState_BOX_STATE_RUNNING},
		Operation: &controlv1.Operation{Id: fmt.Sprint("op-", f.ops), BoxId: id},
	}
}

// fakeResolver routes every box of its owner to the fake agent.
type fakeResolver struct {
	users  *users.Service
	addr   string
	waking *wakeGate
}

func (r fakeResolver) Resolve(ctx context.Context, user, box string) (machines.Endpoint, string, error) {
	if !r.users.HasBox(user, box) {
		return machines.Endpoint{}, "", status.Error(codes.NotFound, "no such box for this user (it may have been deleted)")
	}
	if r.waking.slow.Load() {
		r.waking.entered <- struct{}{}
		select {
		case <-ctx.Done():
			return machines.Endpoint{}, "", status.FromContextError(ctx.Err()).Err()
		case <-time.After(5 * time.Second):
		}
	}
	return machines.TCPEndpoint(r.addr), "w1", nil
}

func (fakeResolver) Reached(string) func() { return func() {} }

type agentLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *agentLog) add(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
}

func (l *agentLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

type wakeGate struct {
	slow    atomic.Bool
	entered chan struct{}
}

type fakeCredentials struct{}

func (fakeCredentials) Credential(box, _ string) string { return "cred:" + box }

func (fakeCredentials) ShareCredential(box, _, principal string) string {
	return "share:" + box + ":" + principal
}

type fakeGW struct{}

func (*fakeGW) Add(context.Context, string) (string, string, error)              { return "gw-key", "ak", nil }
func (*fakeGW) Revoke(context.Context, string) error                             { return nil }
func (*fakeGW) SetEnabled(context.Context, string, bool) error                   { return nil }
func (*fakeGW) Usage(context.Context, string, time.Time) ([]gwkeys.Usage, error) { return nil, nil }

func (*fakeGW) DailyCap(context.Context, string) (*int64, error) { return nil, nil }

func (*fakeGW) DailyCaps(context.Context) (map[string]int64, error) { return nil, nil }
func (*fakeGW) SetDailyCap(context.Context, string, *int64) error   { return nil }

type fakeTenants struct{}

func (fakeTenants) PutTenant(context.Context, string, string, int) error { return nil }
func (fakeTenants) DeleteTenant(context.Context, string) error           { return nil }

// newFakeAgent answers every MCP request over h2c, opens a session on initialize, logs initializes and deletes to seen, and echoes the credential and authorization it saw, and in its text the credential and the body.
func newFakeAgent(t *testing.T, seen *agentLog) *httptest.Server {
	t.Helper()
	agent := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodDelete:
			seen.add("DELETE " + r.Header.Get("Mcp-Session-Id"))
		case strings.Contains(string(body), `"initialize"`):
			seen.add("initialize")
		}
		w.Header().Set("X-Seen-Credential", r.Header.Get("X-Cocoonbox-Credential"))
		w.Header().Set("X-Seen-Authorization", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"initialize"`) {
			w.Header().Set("Mcp-Session-Id", "agent-session")
		}
		text, _ := json.Marshal(r.Header.Get("X-Cocoonbox-Credential") + " " + string(body))
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":` + string(text) + `}]}}`))
	}))
	agent.Config.Protocols = new(http.Protocols)
	agent.Config.Protocols.SetUnencryptedHTTP2(true)
	agent.Start()
	t.Cleanup(agent.Close)
	return agent
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func masterKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return path
}

func lifetime(us *users.Service, id, credential string) context.Context {
	ctx, _ := us.Lifetime(id, credential)
	return ctx
}

func freshDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("COCOONBOX_TEST_DSN")
	if base == "" {
		t.Skip("COCOONBOX_TEST_DSN names no PostgreSQL server")
	}
	conn, err := pgx.Connect(t.Context(), base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	name := "t_" + strings.ToLower(rand.Text()[:12])
	if _, err := conn.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), base); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse COCOONBOX_TEST_DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
