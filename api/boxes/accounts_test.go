package boxes

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/console"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/gwkeys"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

const rootPassword = "root-password"

func TestDisablingAUserHibernatesItsBoxesEndsItsStreamsAndStopsItsKeysUntilEnabled(t *testing.T) {
	e := newDisableEnv(t)
	ctx, cancel := context.WithTimeout(e.as(t), 10*time.Second)
	defer cancel()
	watch, err := e.client.Watch(ctx, &controlv1.WatchBoxesRequest{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if list, err := watch.Recv(); err != nil || len(list.GetBoxes()) != 1 || list.GetBoxes()[0].GetState() != controlv1.BoxState_BOX_STATE_RUNNING {
		t.Fatalf("first Watch answer = %v, %v", list, err)
	}
	e.svc.refreshCredentials(t.Context())
	if got := e.connect.owners(); !slices.Equal(got, []string{e.user}) {
		t.Fatalf("credential reads while enabled = %v", got)
	}

	e.setDisabled(t, true)
	if m, _ := e.fake.Get(t.Context(), e.box); m.State != machines.StateHibernating {
		t.Errorf("the box after disable is %v, want hibernating", m.State)
	}
	for err == nil {
		_, err = watch.Recv()
	}
	if status.Code(err) != codes.Canceled {
		t.Errorf("the open Watch after disable ended with %v, want Canceled", err)
	}
	if !e.gw.disabled(e.user) {
		t.Error("the gw key still works after disable")
	}
	e.svc.refreshCredentials(t.Context())
	if got := e.connect.owners(); len(got) != 1 {
		t.Errorf("credential reads after disable = %v, want none new", got)
	}
	if _, err := e.client.Ensure(e.as(t), &controlv1.EnsureRequest{BoxId: e.box}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Ensure of a disabled user = %v, want Unauthenticated", err)
	}

	e.setDisabled(t, false)
	if e.gw.disabled(e.user) {
		t.Error("the gw key stays off after enable")
	}
	if m, _ := e.fake.Get(t.Context(), e.box); m.State != machines.StateHibernating {
		t.Errorf("enable woke the box: %v", m.State)
	}
	e.fake.setState(e.box, machines.StateHibernated)
	if resp, err := e.client.Ensure(e.as(t), &controlv1.EnsureRequest{BoxId: e.box}); err != nil || resp.GetOperation().GetId() == "" {
		t.Fatalf("Ensure after enable = %v, %v", resp, err)
	}
	if m, _ := e.fake.Get(t.Context(), e.box); m.State != machines.StateStarting {
		t.Errorf("the box after Ensure is %v, want starting", m.State)
	}
}

type disableEnv struct {
	svc     *Service
	fake    *fakeMachines
	connect *fakeConnect
	gw      *fakeGW
	web     *console.Server
	client  controlv1.BoxesClient
	session *http.Cookie
	user    string
	token   string
	box     string
}

func newDisableEnv(t *testing.T) *disableEnv {
	t.Helper()
	gw := &fakeGW{off: map[string]bool{}}
	us, err := users.Open(t.Context(), users.Options{DSN: clustertest.EmptyDSN(t), MasterKeyFile: masterKey(t), GW: gw, Tenants: fakeTenants{}, FirstBoxID: BoxID})
	if err != nil {
		t.Fatalf("open users: %v", err)
	}
	t.Cleanup(us.Close)
	if _, err := us.Create(t.Context(), users.NewUser{Name: "root", Password: rootPassword, Admin: true}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	u, err := us.Create(t.Context(), users.NewUser{Name: "bob"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := us.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	fake := newFakeMachines()
	box := BoxID(u.ID)
	fake.put(machines.Machine{ID: box, Owner: u.ID, State: machines.StateRunning, Flavor: defaultFlavor, Revision: 1})
	svc := New(fake, testDefaults, us)
	svc.EnableAccounts(us)
	conn := &fakeConnect{}
	svc.EnableConnect(conn)

	authn := auth.New(us, us)
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(authn.Unary), grpc.ChainStreamInterceptor(authn.Stream))
	controlv1.RegisterBoxesServer(server, svc)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	e := &disableEnv{
		svc: svc, fake: fake, connect: conn, gw: gw, web: console.New(us, svc, 5_000_000, 1), client: controlv1.NewBoxesClient(cc),
		user: u.ID, token: token, box: box,
	}
	resp := e.consoleCall(t, http.MethodPost, "api/login", `{"name":"root","password":"`+rootPassword+`"}`)
	if resp.Code != http.StatusOK || len(resp.Result().Cookies()) == 0 {
		t.Fatalf("admin login = %d %s", resp.Code, resp.Body)
	}
	e.session = resp.Result().Cookies()[0]
	return e
}

func (e *disableEnv) as(t *testing.T) context.Context {
	return metadata.AppendToOutgoingContext(t.Context(), auth.HeaderAuthorization, "Bearer "+e.token)
}

func (e *disableEnv) setDisabled(t *testing.T, disabled bool) {
	t.Helper()
	if resp := e.consoleCall(t, http.MethodPatch, "api/users/"+e.user, fmt.Sprintf(`{"disabled":%t}`, disabled)); resp.Code != http.StatusOK {
		t.Fatalf("PATCH disabled %t = %d %s", disabled, resp.Code, resp.Body)
	}
}

func (e *disableEnv) consoleCall(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, console.Prefix+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if e.session != nil {
		req.AddCookie(e.session)
	}
	rec := httptest.NewRecorder()
	e.web.ServeHTTP(rec, req)
	return rec
}

type fakeGW struct {
	mu  sync.Mutex
	off map[string]bool
}

func (*fakeGW) Add(_ context.Context, owner string) (string, string, error) {
	return "gw-" + owner, "sha256:" + owner, nil
}

func (*fakeGW) Revoke(context.Context, string) error { return nil }

func (g *fakeGW) SetEnabled(_ context.Context, owner string, enabled bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.off[owner] = !enabled
	return nil
}

func (*fakeGW) Usage(context.Context, string, time.Time) ([]gwkeys.Usage, error) { return nil, nil }

func (*fakeGW) DailyCap(context.Context, string) (*int64, error) { return nil, nil }

func (*fakeGW) DailyCaps(context.Context) (map[string]int64, error) { return nil, nil }

func (*fakeGW) SetDailyCap(context.Context, string, *int64) error { return nil }

func (g *fakeGW) disabled(owner string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.off[owner]
}

type fakeTenants struct{}

func (fakeTenants) PutTenant(context.Context, string, string, int) error { return nil }

func (fakeTenants) DeleteTenant(context.Context, string) error { return nil }

func masterKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return path
}
