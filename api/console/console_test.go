package console

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/gwkeys"
	"github.com/CMGS/cocoonbox/api/users"
)

const password = "correct horse battery"

func TestLoginSetsAStrictCookieAndLimitsFailures(t *testing.T) {
	env := newEnv(t)
	resp := env.do(t, http.MethodPost, "api/login", "", `{"name":"root","password":"`+password+`"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("login = %d %s", resp.Code, resp.Body)
	}
	cookie := resp.Result().Cookies()[0]
	if cookie.Name != cookieName || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != Prefix {
		t.Errorf("cookie = %+v", cookie)
	}
	for i := range nameLimit {
		if code := env.do(t, http.MethodPost, "api/login", "", `{"name":"root","password":"wrong password!"}`).Code; code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, code)
		}
	}
	if code := env.do(t, http.MethodPost, "api/login", "", `{"name":"root","password":"`+password+`"}`).Code; code != http.StatusTooManyRequests {
		t.Errorf("login after %d failures = %d, want 429", nameLimit, code)
	}
	for i := range addrLimit {
		env.do(t, http.MethodPost, "api/login", "", `{"name":"nobody`+strings.Repeat("x", i)+`","password":"wrong password!"}`)
	}
	if code := env.do(t, http.MethodPost, "api/login", "", `{"name":"qa9","password":"`+password+`"}`).Code; code != http.StatusTooManyRequests {
		t.Errorf("login from an address over its limit = %d, want 429", code)
	}
}

func TestParallelSignInsCannotPassTheLimitTogether(t *testing.T) {
	env := newEnv(t)
	codes := make(chan int, 30)
	var wg sync.WaitGroup
	for range cap(codes) {
		wg.Go(func() {
			codes <- env.do(t, http.MethodPost, "api/login", "", `{"name":"root","password":"wrong password!"}`).Code
		})
	}
	wg.Wait()
	close(codes)
	checked := 0
	for code := range codes {
		if code == http.StatusUnauthorized {
			checked++
		}
	}
	if checked > nameLimit {
		t.Errorf("%d parallel wrong passwords reached the check, want at most %d", checked, nameLimit)
	}
}

func TestChangingThePasswordIsLimited(t *testing.T) {
	env := newEnv(t)
	root := env.login(t, "root")
	for range nameLimit {
		env.do(t, http.MethodPost, "api/me/password", root, `{"current":"wrong password!","password":"another password"}`)
	}
	if code := env.do(t, http.MethodPost, "api/me/password", root, `{"current":"`+password+`","password":"another password"}`).Code; code != http.StatusTooManyRequests {
		t.Errorf("password change after %d wrong ones = %d, want 429", nameLimit, code)
	}
}

func TestAdminCreatesAUserWhoGeneratesTheAppToken(t *testing.T) {
	env := newEnv(t)
	root := env.login(t, "root")
	resp := env.do(t, http.MethodPost, "api/users", root, `{"name":"qa1","password":"`+password+`"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("create = %d %s", resp.Code, resp.Body)
	}
	var created users.User
	_ = json.Unmarshal(resp.Body.Bytes(), &created)

	qa1 := env.login(t, "qa1")
	if code := env.do(t, http.MethodGet, "api/users", qa1, "").Code; code != http.StatusForbidden {
		t.Errorf("non-admin user list = %d, want 403", code)
	}
	resp = env.do(t, http.MethodPost, "api/me/token", qa1, "{}")
	var token struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &token); err != nil || token.Token == "" {
		t.Fatalf("token = %d %s", resp.Code, resp.Body)
	}
	sum := sha256.Sum256([]byte(token.Token))
	if id, ok := env.users.ByTokenSHA256(hex.EncodeToString(sum[:])); !ok || id != created.ID {
		t.Errorf("the generated token resolves to %q, %t", id, ok)
	}

	env.boxes.list = []*controlv1.Box{{Id: "cb-1", State: controlv1.BoxState_BOX_STATE_RUNNING, Image: "cbdesk:v13"}}
	resp = env.do(t, http.MethodGet, "api/me", qa1, "")
	body := resp.Body.String()
	if resp.Code != http.StatusOK || !strings.Contains(body, `"state":"running"`) || !strings.Contains(body, `"model":"m1"`) || strings.Contains(body, token.Token) {
		t.Errorf("me = %d %s", resp.Code, body)
	}
	if env.boxes.listedFor != created.ID {
		t.Errorf("boxes listed for %q, want %q", env.boxes.listedFor, created.ID)
	}
	for _, leak := range []string{"gw-", env.gw.keys[created.ID]} {
		if strings.Contains(body, leak) {
			t.Errorf("me shows a backing key: %s", body)
		}
	}
	if code := env.do(t, http.MethodPatch, "api/me", qa1, `{"max_boxes":2}`).Code; code != http.StatusBadRequest {
		t.Errorf("max boxes over the cap = %d, want 400", code)
	}
	if code := env.do(t, http.MethodPatch, "api/me", qa1, `{"max_boxes_cap":5,"residential":true}`).Code; code != http.StatusOK {
		t.Errorf("residential flip = %d", code)
	}
	if u, _ := env.users.Get(created.ID); u.MaxBoxesCap != 1 || !u.Residential {
		t.Errorf("user after self update = %+v, want the cap untouched and residential on", u)
	}
	if code := env.do(t, http.MethodPatch, "api/users/"+created.ID, root, `{"max_boxes_cap":3,"max_boxes":2}`).Code; code != http.StatusOK {
		t.Errorf("admin cap change = %d", code)
	}
	if env.tenants.tenants[created.ID].max != 3 {
		t.Errorf("tenant cap = %d, want 3", env.tenants.tenants[created.ID].max)
	}
}

func TestDeleteReleasesTheBoxesBeforeTheUser(t *testing.T) {
	env := newEnv(t)
	root := env.login(t, "root")
	u, _ := env.users.Create(t.Context(), users.NewUser{Name: "qa1", Password: password})
	token, _ := env.users.RegenerateToken(t.Context(), u.ID)
	qa1 := env.login(t, "qa1")
	env.boxes.onRelease = func() {
		sum := sha256.Sum256([]byte(token))
		if _, ok := env.users.ByTokenSHA256(hex.EncodeToString(sum[:])); ok {
			t.Error("the user's token still worked while its boxes were released")
		}
	}
	if code := env.do(t, http.MethodDelete, "api/users/"+u.ID, root, "{}").Code; code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if env.boxes.released != u.ID {
		t.Errorf("released %q, want %q", env.boxes.released, u.ID)
	}
	if _, ok := env.tenants.tenants[u.ID]; ok {
		t.Error("tenant kept")
	}
	if code := env.do(t, http.MethodGet, "api/me", qa1, "").Code; code != http.StatusUnauthorized {
		t.Errorf("deleted user's session = %d, want 401", code)
	}
	self, _ := env.users.ByName("root")
	if code := env.do(t, http.MethodDelete, "api/users/"+self.ID, root, "{}").Code; code != http.StatusBadRequest {
		t.Errorf("deleting yourself = %d, want 400", code)
	}
}

func TestRequestsNeedASessionAndJSON(t *testing.T) {
	env := newEnv(t)
	if code := env.do(t, http.MethodGet, "api/me", "", "").Code; code != http.StatusUnauthorized {
		t.Errorf("me without a session = %d", code)
	}
	req := httptest.NewRequest(http.MethodPost, Prefix+"api/login", strings.NewReader("name=root&password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form login = %d, want 415", rec.Code)
	}
	rec = httptest.NewRecorder()
	env.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app.js") {
		t.Errorf("index = %d", rec.Code)
	}
}

type env struct {
	srv     *Server
	users   *users.Service
	boxes   *fakeBoxes
	gw      *fakeGW
	tenants *fakeTenants
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := os.Getenv("COCOONBOX_TEST_DSN")
	if base == "" {
		t.Skip("COCOONBOX_TEST_DSN names no PostgreSQL server")
	}
	name := "t_" + randomHex(6)
	if err := exec(base, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _ = exec(base, "DROP DATABASE "+name+" WITH (FORCE)") })
	u, _ := url.Parse(base)
	u.Path = "/" + name
	keyFile := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	e := &env{boxes: &fakeBoxes{}, gw: &fakeGW{keys: map[string]string{}}, tenants: &fakeTenants{tenants: map[string]tenant{}}}
	us, err := users.Open(t.Context(), users.Options{DSN: u.String(), MasterKeyFile: keyFile, GW: e.gw, Tenants: e.tenants})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(us.Close)
	if _, err := us.Create(t.Context(), users.NewUser{Name: "root", Password: password, Admin: true}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	e.users, e.srv = us, New(us, e.boxes)
	return e
}

func (e *env) login(t *testing.T, name string) string {
	t.Helper()
	resp := e.do(t, http.MethodPost, "api/login", "", `{"name":"`+name+`","password":"`+password+`"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("login %s = %d %s", name, resp.Code, resp.Body)
	}
	return resp.Result().Cookies()[0].Value
}

func (e *env) do(t *testing.T, method, path, session, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, Prefix+path, strings.NewReader(body))
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: session})
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

type fakeBoxes struct {
	list      []*controlv1.Box
	listedFor string
	released  string
	onRelease func()
}

func (f *fakeBoxes) List(ctx context.Context, _ *controlv1.ListBoxesRequest) (*controlv1.ListBoxesResponse, error) {
	f.listedFor = auth.User(ctx)
	return &controlv1.ListBoxesResponse{Boxes: f.list}, nil
}

func (f *fakeBoxes) Release(_ context.Context, user string) error {
	if f.onRelease != nil {
		f.onRelease()
	}
	f.released = user
	return nil
}

type fakeGW struct {
	mu   sync.Mutex
	keys map[string]string
}

func (g *fakeGW) Add(_ context.Context, owner string) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.keys[owner] = "gw-" + randomHex(8)
	return g.keys[owner], "sha256:" + owner, nil
}

func (g *fakeGW) Revoke(_ context.Context, owner string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.keys, owner)
	return nil
}

func (g *fakeGW) Usage(context.Context, string) ([]gwkeys.Usage, error) {
	return []gwkeys.Usage{{Model: "m1", Requests: 1, TotalTokens: 10, CostMicros: 5}}, nil
}

type tenant struct {
	token string
	max   int
}

type fakeTenants struct {
	mu      sync.Mutex
	tenants map[string]tenant
}

func (f *fakeTenants) Put(_ context.Context, name, token string, maxClaims int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants[name] = tenant{token: cmp.Or(token, f.tenants[name].token), max: maxClaims}
	return nil
}

func (f *fakeTenants) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tenants, name)
	return nil
}

func exec(dsn, sql string) error {
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	_, err = conn.Exec(context.Background(), sql)
	return err
}

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}
