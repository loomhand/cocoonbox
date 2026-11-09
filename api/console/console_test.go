package console

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/boxes"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/users"
	"github.com/loomhand/cocoonbox/api/users/userstest"
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
	results := make(chan int, 30)
	var wg sync.WaitGroup
	for range cap(results) {
		wg.Go(func() {
			results <- env.do(t, http.MethodPost, "api/login", "", `{"name":"root","password":"wrong password!"}`).Code
		})
	}
	wg.Wait()
	close(results)
	checked := 0
	for code := range results {
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
	if id, ok := env.users.ByTokenSHA256(t.Context(), auth.TokenCredential(token.Token)); !ok || id != created.ID {
		t.Errorf("the generated token resolves to %q, %t", id, ok)
	}
	if code := env.do(t, http.MethodDelete, "api/me/token", qa1, "{}").Code; code != http.StatusNoContent {
		t.Errorf("revoke token = %d, want 204", code)
	}
	if _, ok := env.users.ByTokenSHA256(t.Context(), auth.TokenCredential(token.Token)); ok {
		t.Error("the revoked token still resolves")
	}
	if u, _ := env.users.Get(created.ID); u.HasToken {
		t.Error("the user still has a token after the revoke")
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
	if strings.Contains(body, "gw-") {
		t.Errorf("me shows a backing key: %s", body)
	}
	if code := env.do(t, http.MethodPatch, "api/me", qa1, `{"max_boxes":2}`).Code; code != http.StatusBadRequest {
		t.Errorf("max boxes over the cap = %d, want 400", code)
	}
	if code := env.do(t, http.MethodPatch, "api/me", qa1, `{"residential":true}`).Code; code != http.StatusForbidden {
		t.Errorf("self residential flip = %d, want 403", code)
	}
	if code := env.do(t, http.MethodPatch, "api/me", qa1, `{"max_boxes_cap":5}`).Code; code != http.StatusOK {
		t.Errorf("self update = %d", code)
	}
	if u, _ := env.users.Get(created.ID); u.MaxBoxesCap != 1 || u.Residential {
		t.Errorf("user after self update = %+v, want the cap untouched and residential off", u)
	}
	if code := env.do(t, http.MethodPatch, "api/users/"+created.ID, root, `{"residential":true}`).Code; code != http.StatusOK {
		t.Errorf("admin residential flip = %d", code)
	}
	if u, _ := env.users.Get(created.ID); !u.Residential {
		t.Errorf("user after admin flip = %+v, want residential on", u)
	}
	if code := env.do(t, http.MethodPatch, "api/users/"+created.ID, root, `{"max_boxes_cap":3,"max_boxes":2}`).Code; code != http.StatusOK {
		t.Errorf("admin cap change = %d", code)
	}
	if env.tenants.Max[created.ID] != 3 {
		t.Errorf("tenant cap = %d, want 3", env.tenants.Max[created.ID])
	}
}

func TestTheUserPageManagesBoxes(t *testing.T) {
	env := newEnv(t)
	u, _ := env.users.Create(t.Context(), users.NewUser{Name: "qa1", Password: password})
	qa1 := env.login(t, "qa1")
	env.boxes.list = []*controlv1.Box{{Id: "cb-1", Name: "main"}}
	if body := env.do(t, http.MethodGet, "api/me", qa1, "").Body.String(); !strings.Contains(body, `{"id":"cb-1","name":"main","state":"unspecified","image":""}`) {
		t.Errorf("me = %s", body)
	}
	if resp := env.do(t, http.MethodPost, "api/boxes", qa1, `{"name":"test"}`); resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"name":"test"`) {
		t.Errorf("create = %d %s", resp.Code, resp.Body)
	}
	if resp := env.do(t, http.MethodPost, "api/boxes", qa1, `{"name":"full"}`); resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "is running") {
		t.Errorf("create past the running limit = %d %s", resp.Code, resp.Body)
	}
	if code := env.do(t, http.MethodPatch, "api/boxes/cb-2", qa1, `{"name":"work"}`).Code; code != http.StatusOK {
		t.Errorf("rename = %d", code)
	}
	if resp := env.do(t, http.MethodDelete, "api/boxes/cb-2", qa1, ""); resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"operation_id":"op-1"`) {
		t.Errorf("delete = %d %s", resp.Code, resp.Body)
	}
	if code := env.do(t, http.MethodDelete, "api/boxes/cb-2", "", "").Code; code != http.StatusUnauthorized {
		t.Errorf("delete signed out = %d", code)
	}
	want := []string{"Create " + u.ID + " test", "Create " + u.ID + " full", "Rename " + u.ID + " cb-2 work", "Delete " + u.ID + " cb-2"}
	if !slices.Equal(env.boxes.calls, want) {
		t.Errorf("calls = %v, want %v", env.boxes.calls, want)
	}
}

func TestOnlyAnAdminSetsTheRunningLimit(t *testing.T) {
	env := newEnv(t)
	root := env.login(t, "root")
	u, _ := env.users.Create(t.Context(), users.NewUser{Name: "qa1", Password: password})
	qa1 := env.login(t, "qa1")
	if body := env.do(t, http.MethodGet, "api/me", qa1, "").Body.String(); !strings.Contains(body, `"max_running":0`) || !strings.Contains(body, `"default_max_running":1`) {
		t.Errorf("me = %s", body)
	}
	if code := env.do(t, http.MethodPatch, "api/me", qa1, `{"max_running":1}`).Code; code != http.StatusForbidden {
		t.Errorf("self running limit = %d, want 403", code)
	}
	if code := env.do(t, http.MethodPatch, "api/users/"+u.ID, root, `{"max_boxes_cap":3,"max_boxes":3,"max_running":3}`).Code; code != http.StatusOK {
		t.Errorf("admin running limit = %d", code)
	}
	if body := env.do(t, http.MethodGet, "api/me", qa1, "").Body.String(); !strings.Contains(body, `"max_running":3`) {
		t.Errorf("me after the admin's change = %s", body)
	}
	if code := env.do(t, http.MethodPatch, "api/users/"+u.ID, root, `{"max_running":4}`).Code; code != http.StatusBadRequest {
		t.Errorf("running limit over max boxes = %d, want 400", code)
	}
	if env.users.MaxRunning(u.ID) != 3 {
		t.Errorf("running limit = %d, want 3", env.users.MaxRunning(u.ID))
	}
}

func TestAdminSetsADailyCapThatTheUserSees(t *testing.T) {
	env := newEnv(t)
	root := env.login(t, "root")
	u, _ := env.users.Create(t.Context(), users.NewUser{Name: "qa1", Password: password})
	qa1 := env.login(t, "qa1")

	var me struct {
		DailyCap        *int64 `json:"daily_cap_micros"`
		DefaultDailyCap int64  `json:"default_daily_cap_micros"`
		SpentToday      int64  `json:"spent_today_micros"`
	}
	resp := env.do(t, http.MethodGet, "api/me", qa1, "")
	if err := json.Unmarshal(resp.Body.Bytes(), &me); err != nil || me.DailyCap != nil || me.DefaultDailyCap != 5_000_000 || me.SpentToday != 5 {
		t.Errorf("me before a cap = %d %s", resp.Code, resp.Body)
	}
	if want := "0|" + strconv.FormatInt(time.Now().UTC().Truncate(24*time.Hour).Unix(), 10) + "|"; env.gw.Since != want {
		t.Errorf("usage windows read = %q, want all time and today, %q", env.gw.Since, want)
	}
	if code := env.do(t, http.MethodPut, "api/users/"+u.ID+"/daily-cap", qa1, `{"micros":1}`).Code; code != http.StatusForbidden {
		t.Errorf("non-admin cap change = %d, want 403", code)
	}
	if code := env.do(t, http.MethodPut, "api/users/"+u.ID+"/daily-cap", root, `{"micros":-5}`).Code; code != http.StatusBadRequest {
		t.Errorf("negative cap = %d, want 400", code)
	}
	for _, tt := range []struct {
		body string
		want string
	}{
		{`{"micros":2500000}`, "2500000"},
		{`{"micros":-1}`, "-1"},
		{`{"micros":null}`, "inherit"},
	} {
		if code := env.do(t, http.MethodPut, "api/users/"+u.ID+"/daily-cap", root, tt.body).Code; code != http.StatusOK {
			t.Fatalf("cap %s = %d", tt.body, code)
		}
		if got := capOf(env.gw, u.ID); got != tt.want {
			t.Errorf("gw cap after %s = %s, want %s", tt.body, got, tt.want)
		}
	}
	_ = env.do(t, http.MethodPut, "api/users/"+u.ID+"/daily-cap", root, `{"micros":2500000}`)
	resp = env.do(t, http.MethodGet, "api/me", qa1, "")
	if err := json.Unmarshal(resp.Body.Bytes(), &me); err != nil || me.DailyCap == nil || *me.DailyCap != 2_500_000 {
		t.Errorf("me after a cap = %s", resp.Body)
	}
	var list []struct {
		Name     string `json:"name"`
		DailyCap *int64 `json:"daily_cap_micros"`
	}
	resp = env.do(t, http.MethodGet, "api/users", root, "")
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil || len(list) != 2 {
		t.Fatalf("users = %d %s", resp.Code, resp.Body)
	}
	wantCaps := map[string]int64{"root": -1, "qa1": 2_500_000}
	for _, row := range list {
		want := wantCaps[row.Name]
		if row.DailyCap == nil || *row.DailyCap != want {
			t.Errorf("listed cap of %s = %v, want %d", row.Name, row.DailyCap, want)
		}
	}
}

func TestDeleteReleasesTheBoxesBeforeTheUser(t *testing.T) {
	env := newEnv(t)
	root := env.login(t, "root")
	u, _ := env.users.Create(t.Context(), users.NewUser{Name: "qa1", Password: password})
	token, err := env.users.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	qa1 := env.login(t, "qa1")
	env.boxes.onRelease = func() {
		if _, ok := env.users.ByTokenSHA256(t.Context(), auth.TokenCredential(token)); ok {
			t.Error("the user's token still worked while its boxes were released")
		}
	}
	if code := env.do(t, http.MethodDelete, "api/users/"+u.ID, root, "{}").Code; code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if env.boxes.released != u.ID {
		t.Errorf("released %q, want %q", env.boxes.released, u.ID)
	}
	if env.boxes.stateGone != u.ID {
		t.Errorf("state deleted for %q, want %q", env.boxes.stateGone, u.ID)
	}
	if _, ok := env.tenants.Max[u.ID]; ok {
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

func TestADeleteWhoseClientLeavesStillRemovesEverything(t *testing.T) {
	env := newEnv(t)
	root := env.login(t, "root")
	u, _ := env.users.Create(t.Context(), users.NewUser{Name: "qa1", Password: password})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	env.boxes.onRelease = cancel
	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, env.request(http.MethodDelete, "api/users/"+u.ID, root, "{}").WithContext(ctx))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if env.boxes.stateGone != u.ID || env.boxes.stateErr != nil {
		t.Errorf("state deleted for %q with ctx error %v, want %q with a live ctx", env.boxes.stateGone, env.boxes.stateErr, u.ID)
	}
	if _, err := env.users.ByName("qa1"); !errors.Is(err, users.ErrNotFound) {
		t.Errorf("user after its client left: %v, want %v", err, users.ErrNotFound)
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

func TestWriteJSONSortsMapKeys(t *testing.T) {
	want := `{"github":true,"invite_only":true,"seats":{"cap":2,"queued":0,"used":1},"signup":false}`
	for range 20 {
		rec := httptest.NewRecorder()
		writeJSON(rec, map[string]any{"signup": false, "github": true, "invite_only": true, "seats": map[string]int{"used": 1, "cap": 2, "queued": 0}})
		if got := rec.Body.String(); got != want {
			t.Fatalf("writeJSON = %s, want %s", got, want)
		}
	}
}

type env struct {
	srv     *Server
	dsn     string
	users   *users.Service
	boxes   *fakeBoxes
	gw      *userstest.GW
	tenants *userstest.Tenants
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := clustertest.EmptyDSN(t)
	e := &env{boxes: &fakeBoxes{}, gw: userstest.NewGW(), tenants: userstest.NewTenants()}
	us, err := users.Open(t.Context(), users.Options{DSN: dsn, MasterKeyFile: userstest.MasterKeyFile(t), GW: e.gw, Tenants: e.tenants, FirstBoxID: boxes.BoxID, EnsureLimit: 4, PasswordCost: bcrypt.MinCost})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(us.Close)
	if _, err := us.Create(t.Context(), users.NewUser{Name: "root", Password: password, Admin: true}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	e.users, e.srv, e.dsn = us, New(us, e.boxes, 5_000_000, 1), dsn
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
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, e.request(method, path, session, body))
	return rec
}

func (e *env) request(method, path, session, body string) *http.Request {
	req := httptest.NewRequest(method, Prefix+path, strings.NewReader(body))
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: session})
	}
	return req
}

type fakeBoxes struct {
	list      []*controlv1.Box
	listedFor string
	released  string
	stateGone string
	stateErr  error
	onRelease func()
	calls     []string
}

func (f *fakeBoxes) List(ctx context.Context, _ *controlv1.ListBoxesRequest) (*controlv1.ListBoxesResponse, error) {
	f.listedFor = auth.User(ctx)
	return &controlv1.ListBoxesResponse{Boxes: f.list}, nil
}

func (f *fakeBoxes) Create(ctx context.Context, req *controlv1.CreateBoxRequest) (*controlv1.EnsureResponse, error) {
	f.calls = append(f.calls, "Create "+auth.User(ctx)+" "+req.GetName())
	if req.GetName() == "full" {
		return nil, status.Error(codes.FailedPrecondition, "box main (cb-1) is running")
	}
	return &controlv1.EnsureResponse{Box: &controlv1.Box{Id: "cb-2", Name: req.GetName(), State: controlv1.BoxState_BOX_STATE_STARTING}}, nil
}

func (f *fakeBoxes) Rename(ctx context.Context, req *controlv1.RenameBoxRequest) (*controlv1.Box, error) {
	f.calls = append(f.calls, "Rename "+auth.User(ctx)+" "+req.GetBoxId()+" "+req.GetName())
	return &controlv1.Box{Id: req.GetBoxId(), Name: req.GetName()}, nil
}

func (f *fakeBoxes) Delete(ctx context.Context, ref *controlv1.BoxRef) (*controlv1.Operation, error) {
	f.calls = append(f.calls, "Delete "+auth.User(ctx)+" "+ref.GetBoxId())
	return &controlv1.Operation{Id: "op-1", BoxId: ref.GetBoxId()}, nil
}

func (f *fakeBoxes) Release(_ context.Context, user string) error {
	if f.onRelease != nil {
		f.onRelease()
	}
	f.released = user
	return nil
}

func (*fakeBoxes) HibernateAll(context.Context, string) error { return nil }

func (f *fakeBoxes) DeleteState(ctx context.Context, user string) error {
	f.stateGone, f.stateErr = user, ctx.Err()
	return nil
}

func capOf(gw *userstest.GW, owner string) string {
	if micros, ok := gw.Caps[owner]; ok {
		return strconv.FormatInt(micros, 10)
	}
	return "inherit"
}
