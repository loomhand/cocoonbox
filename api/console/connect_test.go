package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/CMGS/cocoonbox/api/connect"
	"github.com/CMGS/cocoonbox/api/connect/connecttest"
	"github.com/CMGS/cocoonbox/api/users"
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
	req := httptest.NewRequest(http.MethodGet, connect.CallbackPath+"?installation_id=1&setup_action=install&code=x", nil)
	rec := httptest.NewRecorder()
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
