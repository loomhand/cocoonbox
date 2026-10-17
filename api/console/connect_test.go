package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/boxes"
	"github.com/loomhand/cocoonbox/api/connect"
	"github.com/loomhand/cocoonbox/api/connect/connecttest"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/users"
	"github.com/loomhand/cocoonbox/api/vault"
)

func TestTheCallbackCompletesOnlyWithTheFlowsNonce(t *testing.T) {
	env, vendor := newConnectEnv(t)
	root := env.login(t, "root")
	if _, err := env.users.Create(t.Context(), users.NewUser{Name: "bob", Password: password}); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	bob := env.login(t, "bob")

	cookie, code, state := env.begin(t, root, "mock", vendor)
	if len(cookie.Value) != 43 || cookie.Value == root || cookie.Path != connect.Prefix || cookie.SameSite != http.SameSiteLaxMode ||
		!cookie.HttpOnly || !cookie.Secure || cookie.MaxAge != 600 {
		t.Errorf("connect cookie = %+v", cookie)
	}
	bobCookie, _, bobState := env.begin(t, bob, "bobs", vendor)
	for name, nonce := range map[string]string{"no cookie": "", "a wrong nonce": strings.Repeat("x", 43), "bob's nonce": bobCookie.Value, "root's session": root} {
		if loc := env.callback(t, nonce, state, code); !strings.Contains(loc, "error=") {
			t.Errorf("callback with %s -> %s", name, loc)
		}
	}
	if loc := env.callback(t, cookie.Value, bobState, code); !strings.Contains(loc, "error=") {
		t.Errorf("callback of bob's flow in root's browser -> %s", loc)
	}
	if loc := env.callback(t, cookie.Value, state, code); loc != connect.ConsolePath+"?connected=mock" {
		t.Fatalf("callback -> %s", loc)
	}
	if loc := env.callback(t, cookie.Value, state, code); !strings.Contains(loc, "error=") {
		t.Errorf("replayed state -> %s", loc)
	}

	cookie, code, state = env.begin(t, root, "mock", vendor)
	if err := exec(env.dsn, "UPDATE connect_flows SET expires_at = now() - interval '1 second' WHERE connector = 'mock'"); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if loc := env.callback(t, cookie.Value, state, code); !strings.Contains(loc, "error=") {
		t.Errorf("expired state -> %s", loc)
	}
	cookie, code, state = env.begin(t, root, "mock", vendor)
	req := httptest.NewRequest(http.MethodGet, connect.CallbackPath+"?"+url.Values{"state": {state}, "code": {code}, "iss": {"https://other.example"}}.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: connectCookie, Value: cookie.Value})
	rec := httptest.NewRecorder()
	env.srv.ConnectCallback(rec, req)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("callback naming another server -> %s", loc)
	}
	req = httptest.NewRequest(http.MethodGet, connect.CallbackPath+"?installation_id=1&setup_action=install&code=x", nil)
	rec = httptest.NewRecorder()
	env.srv.ConnectCallback(rec, req)
	if loc := rec.Header().Get("Location"); loc != connect.ConsolePath+"?installed=github" {
		t.Errorf("GitHub install -> %s", loc)
	}

	var state1 struct {
		Connectors []connector `json:"connectors"`
	}
	resp := env.do(t, http.MethodGet, "api/connect", root, "")
	if err := json.Unmarshal(resp.Body.Bytes(), &state1); err != nil || len(state1.Connectors) != 1 || len(state1.Connectors[0].Tools) != 2 {
		t.Fatalf("connect state = %s", resp.Body)
	}
	if strings.Contains(resp.Body.String(), "at-") || strings.Contains(resp.Body.String(), "rt-") {
		t.Errorf("the page's state carries a token: %s", resp.Body)
	}
	resp = env.do(t, http.MethodGet, "api/connect", bob, "")
	if !strings.Contains(resp.Body.String(), `"connectors":[]`) {
		t.Errorf("bob sees %s", resp.Body)
	}
}

func TestASignOutBetweenBeginAndCallbackRefusesTheCallback(t *testing.T) {
	env, vendor := newConnectEnv(t)
	root := env.login(t, "root")
	cookie, code, state := env.begin(t, root, "mock", vendor)
	if resp := env.do(t, http.MethodPost, "api/logout", root, "{}"); resp.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", resp.Code)
	}
	if loc := env.callback(t, cookie.Value, state, code); !strings.Contains(loc, "error=") {
		t.Errorf("callback after the sign-out -> %s", loc)
	}
	root = env.login(t, "root")
	if resp := env.do(t, http.MethodGet, "api/connect", root, ""); !strings.Contains(resp.Body.String(), `"connectors":[]`) {
		t.Errorf("connectors after a refused callback: %s", resp.Body)
	}
}

func TestTheConnectPageSetsPoliciesAndDisconnects(t *testing.T) {
	env, vendor := newConnectEnv(t)
	root := env.login(t, "root")
	cookie, code, state := env.begin(t, root, "mock", vendor)
	env.callback(t, cookie.Value, state, code)

	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, connect.ConsolePath, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "connect.js") || rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("page = %d %v", rec.Code, rec.Header())
	}
	if resp := env.do(t, http.MethodPut, "api/connect/mock/policy", root, `{"tool":"create_item","policy":"off"}`); resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"policy":"off"`) {
		t.Errorf("policy = %d %s", resp.Code, resp.Body)
	}
	if resp := env.do(t, http.MethodPut, "api/connect/mock/policy", root, `{"tool":"create_item","policy":"never"}`); resp.Code != http.StatusBadRequest {
		t.Errorf("bad policy = %d", resp.Code)
	}
	if resp := env.do(t, http.MethodDelete, "api/connect/mock", root, ""); resp.Code != http.StatusOK || resp.Body.String() != "{\"revoked\":true}\n" {
		t.Errorf("disconnect = %d %s", resp.Code, resp.Body)
	}
	if resp := env.do(t, http.MethodGet, "api/connect/activity", root, ""); !strings.Contains(resp.Body.String(), `"decision":"disconnect"`) {
		t.Errorf("activity = %s", resp.Body)
	}
	if resp := env.do(t, http.MethodDelete, "api/connect/mock", root, ""); resp.Code != http.StatusNotFound {
		t.Errorf("second disconnect = %d", resp.Code)
	}
	if resp := env.do(t, http.MethodPost, "api/connect/mock/begin", "", "{}"); resp.Code != http.StatusUnauthorized {
		t.Errorf("begin without a session = %d", resp.Code)
	}
}

func TestDeleteRevokesTheGrantsAndLeavesNoRowOfTheUser(t *testing.T) {
	env, vendor := newConnectEnv(t)
	keys, err := vault.Open(t.Context(), vault.Options{DSN: env.dsn, Keys: env.users})
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	t.Cleanup(keys.Close)
	root := env.login(t, "root")
	cookie, code, state := env.begin(t, root, "mock", vendor)
	env.callback(t, cookie.Value, state, code)
	u, err := env.users.Create(t.Context(), users.NewUser{Name: "qa1", Password: password})
	if err != nil {
		t.Fatalf("create qa1: %v", err)
	}
	qa1 := env.login(t, "qa1")
	cookie, code, state = env.begin(t, qa1, "mock", vendor)
	env.callback(t, cookie.Value, state, code)
	env.begin(t, qa1, "pending", vendor)
	for _, machine := range []string{"cb-m1", "cb-m2"} {
		if _, err := env.srv.connect.DoorKey(t.Context(), machine, u.ID, "inc1"); err != nil {
			t.Fatalf("DoorKey: %v", err)
		}
	}
	if _, err := keys.Add(auth.WithUser(t.Context(), u.ID), &controlv1.AddKeyRequest{Name: "K", Hosts: []string{"a.example.com"}, Value: "sk-proj-synthetic0123456789abcdef"}); err != nil {
		t.Fatalf("vault Add: %v", err)
	}
	wake := users.WakeSchedule{Box: boxes.BoxID(u.ID), Owner: u.ID, Cron: "0 9 * * *", TZ: "UTC", AwakeFor: time.Hour, NextAt: time.Now().Add(time.Hour)}
	if err := env.users.PutWakeSchedule(t.Context(), wake, 1); err != nil {
		t.Fatalf("PutWakeSchedule: %v", err)
	}
	if err := exec(env.dsn, `INSERT INTO connect_approvals (id, user_id, grant_id, tool, args_sha256, summary, expires_at)
		SELECT 'ca_qa1', user_id, id, 'create_item', 'x', 's', now() + interval '10 minutes' FROM connect_grants WHERE user_id = '`+u.ID+`';
		INSERT INTO connect_audit (user_id, connector, tool, door, caller, decision, outcome) VALUES ('`+u.ID+`', 'mock', 'list_items', 'mcp', 'mcp:test', 'allow', 'ok')`); err != nil {
		t.Fatalf("approval and audit: %v", err)
	}
	tables := map[string]string{
		"users": "id", "sessions": "user_id", "boxes": "owner", "wake_schedules": "owner", "vault_secrets": "owner", "vault_audit": "owner",
		"connect_grants": "user_id", "connect_flows": "user_id", "connect_approvals": "user_id", "connect_audit": "user_id", "connect_door_keys": "user_id",
	}
	for table, column := range tables {
		if n := countRows(t, env.dsn, table, column, u.ID); n == 0 {
			t.Fatalf("%s holds no row of qa1 before the delete", table)
		}
	}
	_, revokedBefore, _ := vendor.Snapshot()

	if code := env.do(t, http.MethodDelete, "api/users/"+u.ID, root, "{}").Code; code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	for table, column := range tables {
		if n := countRows(t, env.dsn, table, column, u.ID); n != 0 {
			t.Errorf("%s holds %d rows of the deleted user", table, n)
		}
	}
	if _, revoked, _ := vendor.Snapshot(); len(revoked) != len(revokedBefore)+1 {
		t.Errorf("vendor revokes = %d, want %d", len(revoked), len(revokedBefore)+1)
	}
	self, _ := env.users.ByName("root")
	if n := countRows(t, env.dsn, "connect_grants", "user_id", self.ID); n != 1 {
		t.Errorf("root's grants = %d, want 1", n)
	}
}

func TestTheConsoleOffersConnectOnlyWhenItIsEnabled(t *testing.T) {
	env := newEnv(t)
	root := env.login(t, "root")
	if body := env.do(t, http.MethodGet, "api/me", root, "").Body.String(); !strings.Contains(body, `"connect":false`) {
		t.Errorf("me with Connect off = %s", body)
	}
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, connect.ConsolePath, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("Connect page with Connect off = %d, want 404", rec.Code)
	}
	env, _ = newConnectEnv(t)
	root = env.login(t, "root")
	if body := env.do(t, http.MethodGet, "api/me", root, "").Body.String(); !strings.Contains(body, `"connect":true`) {
		t.Errorf("me with Connect on = %s", body)
	}
}

func newConnectEnv(t *testing.T) (*env, *connecttest.Vendor) {
	t.Helper()
	e := newEnv(t)
	vendor := connecttest.New()
	t.Cleanup(vendor.Close)
	broker, err := connect.Open(t.Context(), connect.Options{DSN: e.dsn, Keys: e.users, Sessions: e.users, PublicURL: "https://cb.example", AllowPrivate: true})
	if err != nil {
		t.Fatalf("connect.Open: %v", err)
	}
	t.Cleanup(broker.Close)
	e.srv.EnableConnect(broker)
	return e, vendor
}

// begin starts connecting the vendor as custom connector name and consents; it returns the connect cookie, the code and the state.
func (e *env) begin(t *testing.T, session, name string, vendor *connecttest.Vendor) (*http.Cookie, string, string) {
	t.Helper()
	resp := e.do(t, http.MethodPost, "api/connect/custom/begin", session, `{"name":"`+name+`","url":"`+vendor.MCPURL()+`"}`)
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil || out.URL == "" {
		t.Fatalf("begin = %d %s", resp.Code, resp.Body)
	}
	code, state, err := vendor.Consent(out.URL)
	if err != nil {
		t.Fatalf("consent: %v", err)
	}
	for _, c := range resp.Result().Cookies() {
		if c.Name == connectCookie {
			return c, code, state
		}
	}
	t.Fatal("begin set no connect cookie")
	return nil, "", ""
}

// callback delivers the vendor's redirect with nonce in the connect cookie and returns where it sends the browser.
func (e *env) callback(t *testing.T, nonce, state, code string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, connect.CallbackPath+"?"+url.Values{"state": {state}, "code": {code}}.Encode(), nil)
	if nonce != "" {
		req.AddCookie(&http.Cookie{Name: connectCookie, Value: nonce})
	}
	rec := httptest.NewRecorder()
	e.srv.ConnectCallback(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d", rec.Code)
	}
	return rec.Header().Get("Location")
}

func countRows(t *testing.T, dsn, table, column, id string) int {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var n int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE "+column+" = $1", id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
