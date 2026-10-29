package console

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/boxes"
	"github.com/loomhand/cocoonbox/api/shares"
	"github.com/loomhand/cocoonbox/api/users"
)

func TestOwnerManagesTheShareOfTheirBox(t *testing.T) {
	env := newShareEnv(t)
	qa1 := env.login(t, "qa1")
	box := boxes.BoxID(env.owner.ID)
	if code := env.do(t, http.MethodPost, "api/shares", qa1, `{"box_id":"`+boxes.BoxID("root")+`"}`).Code; code != http.StatusNotFound {
		t.Errorf("sharing another user's box = %d, want 404", code)
	}
	created := env.createShare(t, qa1, `{"box_id":"`+box+`","user_names":["qa2"],"ttl_seconds":3600}`)
	if created.Share.Box != box || len(created.Share.Users) != 1 || created.Share.Users[0] != "qa2" || created.Share.Write {
		t.Errorf("created %+v", created.Share)
	}
	if code := env.do(t, http.MethodPost, "api/shares", qa1, `{"box_id":"`+box+`"}`).Code; code != http.StatusConflict {
		t.Errorf("a second share = %d, want 409", code)
	}
	resp := env.do(t, http.MethodPatch, "api/shares/"+created.Share.ID, qa1, `{"write":true}`)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"write":true`) {
		t.Errorf("allow write = %d %s", resp.Code, resp.Body)
	}
	if code := env.do(t, http.MethodPatch, "api/shares/"+created.Share.ID, env.login(t, "qa2"), `{"write":false}`).Code; code != http.StatusNotFound {
		t.Errorf("a guest switching write = %d, want 404", code)
	}
	if list := env.listShares(t, qa1); len(list) != 1 || list[0].Secret != created.Secret {
		t.Errorf("the owner's list misses the link: %d shares", len(list))
	}
	if list := env.do(t, http.MethodGet, "api/shares", env.login(t, "qa2"), "").Body.String(); strings.Contains(list, created.Secret) || strings.TrimSpace(list) != "[]" {
		t.Error("a guest's list holds the share or its secret")
	}
	resp = env.do(t, http.MethodPost, "api/shares/"+created.Share.ID+"/regenerate", qa1, "{}")
	if resp.Code != http.StatusOK || strings.Contains(resp.Body.String(), created.Secret) {
		t.Errorf("regenerate = %d", resp.Code)
	}
	authn := auth.New(env.users, env.users)
	if _, err := authn.Admit(created.Secret, box, env.qa2.ID); status.Code(err) != codes.PermissionDenied {
		t.Errorf("the old secret after regenerate: %v, want PermissionDenied", err)
	}
	list := env.listShares(t, qa1)
	if len(list) != 1 || list[0].Secret == created.Secret || !strings.HasPrefix(list[0].Secret, auth.SecretPrefix) {
		t.Fatal("the owner's list keeps the old link or has none")
	}
	if _, err := authn.Admit(list[0].Secret, box, env.qa2.ID); err != nil {
		t.Errorf("the listed link after regenerate: %v", err)
	}
	if code := env.do(t, http.MethodDelete, "api/shares/"+created.Share.ID, qa1, "{}").Code; code != http.StatusNoContent {
		t.Errorf("revoke = %d", code)
	}
	if list := env.do(t, http.MethodGet, "api/shares", qa1, "").Body.String(); strings.TrimSpace(list) != "[]" {
		t.Errorf("after revoke the list is %s", list)
	}
}

func TestSharePageAdmitsTheSecretAndNamedViewers(t *testing.T) {
	env := newShareEnv(t)
	qa1, qa2, qa3 := env.login(t, "qa1"), env.login(t, "qa2"), env.login(t, "qa3")
	box := boxes.BoxID(env.owner.ID)
	named := env.createShare(t, qa1, `{"box_id":"`+box+`","user_names":["qa2"]}`)
	srv := httptest.NewServer(env.srv)
	t.Cleanup(srv.Close)

	rec := httptest.NewRecorder()
	env.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, sharePrefix, nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "share.js") || !strings.Contains(csp, "connect-src 'self' wss://example.com") {
		t.Errorf("share page = %d, CSP %q", rec.Code, csp)
	}
	for _, path := range []string{"novnc/core/rfb.js", "novnc/LICENSE.txt", "novnc/docs/LICENSE.MPL-2.0"} {
		rec = httptest.NewRecorder()
		env.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, sharePrefix+path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d", path, rec.Code)
		}
	}

	tests := []struct {
		name, secret, session, want string
	}{
		{name: "named viewer", secret: named.Secret, session: qa2, want: "ok"},
		{name: "not signed in", secret: named.Secret, want: "unauthenticated"},
		{name: "another user", secret: named.Secret, session: qa3, want: "permissiondenied"},
		{name: "bad secret", secret: auth.SecretPrefix + "nope", session: qa2, want: "permissiondenied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := dialShare(t, srv.URL, srv.URL, tt.session)
			if err := websocket.Message.Send(ws, tt.secret); err != nil {
				t.Fatalf("send secret: %v", err)
			}
			var reply struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := websocket.JSON.Receive(ws, &reply); err != nil {
				t.Fatalf("reply: %v", err)
			}
			if got := map[bool]string{true: "ok", false: reply.Code}[reply.OK]; got != tt.want {
				t.Fatalf("reply %+v, want %s", reply, tt.want)
			}
			if !reply.OK {
				return
			}
			var banner []byte
			if err := websocket.Message.Receive(ws, &banner); err != nil || string(banner) != "RFB 003.008\n" {
				t.Fatalf("screen bytes %q, %v", banner, err)
			}
			if _, err := ws.Write([]byte("viewer bytes")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := <-env.screens.got; got.principal != "share:"+named.Share.ID+":read:"+env.qa2.ID || got.data != "viewer bytes" {
				t.Errorf("bridge got %+v", got)
			}
		})
	}
	if _, err := websocket.Dial(strings.Replace(srv.URL, "http", "ws", 1)+sharePrefix+"ws", "", "https://evil.example"); err == nil {
		t.Error("a WebSocket from another origin was accepted")
	}
}

func TestANamedViewersScreenEndsWhenTheViewerSignsOutEverywhere(t *testing.T) {
	env := newShareEnv(t)
	env.screens.hold = true
	qa1, qa2 := env.login(t, "qa1"), env.login(t, "qa2")
	named := env.createShare(t, qa1, `{"box_id":"`+boxes.BoxID(env.owner.ID)+`","user_names":["qa2"]}`)
	srv := httptest.NewServer(env.srv)
	t.Cleanup(srv.Close)
	ws := dialShare(t, srv.URL, srv.URL, qa2)
	if err := websocket.Message.Send(ws, named.Secret); err != nil {
		t.Fatalf("send secret: %v", err)
	}
	var banner []byte
	if err := websocket.Message.Receive(ws, &banner); err != nil {
		t.Fatalf("screen: %v", err)
	}
	if err := env.users.SignOut(t.Context(), env.qa2.ID); err != nil {
		t.Fatalf("SignOut: %v", err)
	}
	select {
	case <-env.screens.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the named viewer's screen outlived the viewer's sign-out")
	}
}

type shareEnv struct {
	*env
	owner   users.User
	qa2     users.User
	screens *fakeScreens
}

func newShareEnv(t *testing.T) *shareEnv {
	t.Helper()
	e := &shareEnv{env: newEnv(t), screens: &fakeScreens{got: make(chan bridged, 4), ended: make(chan struct{}, 1)}}
	for _, name := range []string{"qa1", "qa2", "qa3"} {
		u, err := e.users.Create(t.Context(), users.NewUser{Name: name, Password: password})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		switch name {
		case "qa1":
			e.owner = u
		case "qa2":
			e.qa2 = u
		}
	}
	e.srv.EnableSharing(shares.New(e.users), auth.New(e.users, e.users), e.screens)
	return e
}

func (e *shareEnv) createShare(t *testing.T, session, body string) (out struct {
	Share  share  `json:"share"`
	Secret string `json:"secret"`
},
) {
	t.Helper()
	resp := e.do(t, http.MethodPost, "api/shares", session, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("create share = %d %s", resp.Code, resp.Body)
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil || !strings.HasPrefix(out.Secret, auth.SecretPrefix) {
		t.Fatalf("create share: %v", err)
	}
	return out
}

func (e *shareEnv) listShares(t *testing.T, session string) []share {
	t.Helper()
	resp := e.do(t, http.MethodGet, "api/shares", session, "")
	var out []share
	if err := json.Unmarshal(resp.Body.Bytes(), &out); resp.Code != http.StatusOK || err != nil {
		t.Fatalf("list shares = %d, %v", resp.Code, err)
	}
	return out
}

func dialShare(t *testing.T, base, origin, session string) *websocket.Conn {
	t.Helper()
	cfg, err := websocket.NewConfig(strings.Replace(base, "http", "ws", 1)+sharePrefix+"ws", origin)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if session != "" {
		cfg.Header = http.Header{"Cookie": {cookieName + "=" + session}}
	}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

type bridged struct {
	principal string
	data      string
}

// fakeScreens sends an RFB banner and reports the principal and the first bytes the viewer sends; hold keeps the screen until its context ends.
type fakeScreens struct {
	got   chan bridged
	hold  bool
	ended chan struct{}
}

func (f *fakeScreens) Screen(ctx context.Context, g auth.Grant, viewer io.ReadWriter) error {
	if _, err := viewer.Write([]byte("RFB 003.008\n")); err != nil {
		return err
	}
	if f.hold {
		<-ctx.Done()
		f.ended <- struct{}{}
		return ctx.Err()
	}
	buf := make([]byte, 64)
	n, err := viewer.Read(buf)
	f.got <- bridged{principal: g.Principal(), data: string(buf[:n])}
	return err
}
