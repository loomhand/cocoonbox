package boxes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/console"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
	"github.com/loomhand/cocoonbox/api/users/userstest"
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
	if !e.gw.Off(e.user) {
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
	if e.gw.Off(e.user) {
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

func TestADisabledOwnerStartsNoMachine(t *testing.T) {
	fake := newFakeMachines()
	registry := newFakeRegistry()
	registry.disabled = map[string]bool{alice: true}
	svc := newService(fake, testDefaults, registry)

	if _, err := svc.Ensure(auth.WithUser(t.Context(), alice), &controlv1.EnsureRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Ensure of a disabled owner = %v, want FailedPrecondition", err)
	}
	if ms, _ := fake.List(t.Context(), alice); len(ms) != 0 {
		t.Errorf("machines of the disabled owner = %v, want none", ms)
	}
}

func TestAStartReadsTheAccountItselfNotTheCache(t *testing.T) {
	for _, tt := range []struct {
		name   string
		stored bool
		err    error
		want   codes.Code
	}{
		{"another replica disabled the owner", true, nil, codes.FailedPrecondition},
		{"the account read fails", false, errors.New("dial tcp db.internal:5432: connection refused"), codes.Unavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeMachines()
			registry := newFakeRegistry()
			registry.stored, registry.accountErr = map[string]bool{alice: tt.stored}, tt.err
			svc := newService(fake, testDefaults, registry)

			_, err := svc.Ensure(auth.WithUser(t.Context(), alice), &controlv1.EnsureRequest{})
			if status.Code(err) != tt.want || strings.Contains(status.Convert(err).Message(), "db.internal") {
				t.Fatalf("Ensure = %v, want %v without the driver's text", err, tt.want)
			}
			if ms, _ := fake.List(t.Context(), alice); len(ms) != 0 {
				t.Errorf("machines = %v, want none", ms)
			}
		})
	}
}

func TestAnIdleSaveYieldsToAnyRunningOperation(t *testing.T) {
	ops := newOperations()
	restoring, _ := ops.start(t.Context(), alice, aliceBox, controlv1.Phase_PHASE_RESTORING_FILES, 1, time.Minute)
	got, started := ops.begin(t.Context(), &operation{user: alice, boxID: aliceBox, phase: controlv1.Phase_PHASE_SAVING, gen: 1, yield: true}, time.Minute)
	if started || got != restoring || ops.activeFor(aliceBox) != restoring {
		t.Fatalf("idle save began = %t with %+v active, want the restore left running", started, ops.activeFor(aliceBox))
	}
}

func TestReleaseWaitsForAStartOfTheUser(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Revision: 1})
	svc := newService(fake, testDefaults, newFakeRegistry())
	unlock, err := svc.lockStarts(t.Context(), alice)
	if err != nil {
		t.Fatalf("lockStarts: %v", err)
	}
	released := make(chan error, 1)
	go func() { released <- svc.Release(t.Context(), alice) }()
	select {
	case err := <-released:
		unlock()
		t.Fatalf("Release = %v while a start holds the user's lock, want it to wait", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if err := <-released; err != nil {
		t.Fatalf("Release: %v", err)
	}
	if ms, _ := fake.List(t.Context(), alice); len(ms) != 0 {
		t.Errorf("machines after Release = %v, want none", ms)
	}
}

type disableEnv struct {
	svc     *Service
	fake    *fakeMachines
	connect *fakeConnect
	gw      *userstest.GW
	web     *console.Server
	client  controlv1.BoxesClient
	session *http.Cookie
	user    string
	token   string
	box     string
}

func newDisableEnv(t *testing.T) *disableEnv {
	t.Helper()
	gw := userstest.NewGW()
	us, err := users.Open(t.Context(), users.Options{DSN: clustertest.EmptyDSN(t), MasterKeyFile: userstest.MasterKeyFile(t), GW: gw, Tenants: userstest.NewTenants(), FirstBoxID: BoxID, EnsureLimit: 4, PasswordCost: bcrypt.MinCost})
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
	fake.put(machines.Machine{ID: box, Owner: u.ID, State: machines.StateRunning, Revision: 1})
	svc := newService(fake, testDefaults, us)
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
