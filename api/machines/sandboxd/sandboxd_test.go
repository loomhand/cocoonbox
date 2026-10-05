package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	rootToken = "root-token"
	boxID     = "cb-0123456789ab"
	owner     = "alice"
	template  = "cbdesk:v8"
)

var xlarge = machines.Flavor{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40}

func TestNewRefusesAFlavorThatIsNoTier(t *testing.T) {
	fake := newFakeSandboxd(t)
	_, err := New(t.Context(), Options{URL: fake.srv.URL, Token: rootToken, Flavors: []machines.Flavor{{VCPUs: 3, MemoryMiB: 8192}}})
	if err == nil || !strings.Contains(err.Error(), "not a sandboxd tier") {
		t.Fatalf("New = %v, want a tier error", err)
	}
}

func TestEnsureClaimsAndReportsRunningOnceTheAgentAnswers(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)

	m, action, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge})
	if err != nil || action != machines.ActionCreated || m.State != machines.StateStarting || m.Revision != 1 {
		t.Fatalf("Ensure = %+v, %v, %v", m, action, err)
	}
	got := waitState(t, b, boxID, m.Revision, machines.StateRunning)
	if got.Image != template || got.Flavor != xlarge || got.Owner != owner || got.Incarnation == "" || got.CreatedAt.IsZero() {
		t.Errorf("running machine = %+v", got)
	}
	claim := fake.lastClaim()
	want := claimRequest{
		poolKey:    poolKey{Template: template, Net: "none", Size: "xlarge"},
		TTLSeconds: 3600, OnExpire: "archive",
		Metadata: map[string]string{"cb_machine": boxID, "cb_owner": owner},
	}
	if fmt.Sprint(claim) != fmt.Sprint(want) {
		t.Errorf("claim = %+v, want %+v", claim, want)
	}
	if again, action, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge}); err != nil || action != machines.ActionNone || again.State != machines.StateRunning {
		t.Errorf("second Ensure = %+v, %v, %v", again, action, err)
	}
	if n := fake.count("claim"); n != 1 {
		t.Errorf("claims = %d, want 1", n)
	}
	if _, _, err := b.Ensure(t.Context(), machines.Spec{ID: "cb-other", Owner: owner, Flavor: machines.Flavor{VCPUs: 3}}); err == nil {
		t.Error("Ensure accepted a flavor that is no tier")
	}
}

func TestIndexRebuildsMachinesFromClaimsAndEnv(t *testing.T) {
	fake := newFakeSandboxd(t)
	claimed := time.UnixMilli(1790000000000).UTC()
	fake.add(&fakeSandbox{
		row: row{
			ID: "sb_old", Key: poolKey{Template: template, Net: "none", Size: "xlarge"}, Restarts: 1, ClaimedAt: claimed,
			Metadata: map[string]string{"cb_machine": boxID, "cb_owner": owner},
		},
		env: map[string]envVar{"CB_META_wake": {Value: "x|w1"}, "CB_META_gw": {Value: "1"}, "CB_WAKES": {Value: "2"}, "GW_KEY": {Guest: new(false)}, "EGRESS_UPSTREAM": {Guest: new(false)}},
	})
	fake.add(&fakeSandbox{row: row{ID: "sb_foreign", Key: poolKey{Template: "base:24.04"}}})
	b := newTestBackend(t, fake)

	ms, err := b.List(t.Context(), owner)
	if err != nil || len(ms) != 1 {
		t.Fatalf("List = %+v, %v", ms, err)
	}
	m := ms[0]
	if m.ID != boxID || m.Incarnation != "1790000000000.1.2" || m.Meta["wake"] != "x|w1" || m.Meta["gw"] != "1" || len(m.Meta) != 2 || m.CreatedAt != claimed || !m.Routed {
		t.Errorf("indexed machine = %+v", m)
	}
	if all, _ := b.List(t.Context(), ""); len(all) != 1 {
		t.Errorf("List all = %+v, want only the cocoonbox claim", all)
	}
	if other, _ := b.List(t.Context(), "bob"); len(other) != 0 {
		t.Errorf("List bob = %+v", other)
	}
	if _, err := b.Get(t.Context(), "cb-missing"); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("Get of an unknown machine = %v", err)
	}
	fake.remove("sb_old")
	if _, err := b.Get(t.Context(), boxID); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("Get after the claim vanished = %v", err)
	}
}

func TestATenantClaimBelongsToItsTenant(t *testing.T) {
	fake := newFakeSandboxd(t)
	tenants, err := NewTenants(fake.srv.URL, rootToken, "desk")
	if err != nil {
		t.Fatalf("NewTenants: %v", err)
	}
	if err := tenants.Put(t.Context(), "u-1", "tenant-token", 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fake.add(&fakeSandbox{row: row{ID: "sb_stamped", Key: poolKey{Template: template, Size: "xlarge"}, Tenant: "u-2", Metadata: map[string]string{"cb_machine": "cb-b", "cb_owner": "u-1"}}})
	b := newTestBackend(t, fake)

	m, _, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: "u-1", Tenant: "tenant-token", Flavor: xlarge})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	waitState(t, b, boxID, m.Revision, machines.StateRunning)
	if sb := fake.sandboxes["sb_1"]; sb == nil || sb.row.Tenant != "u-1" {
		t.Fatalf("claim = %+v, want it made with the tenant token", sb)
	}
	if ms, _ := b.List(t.Context(), "u-1"); len(ms) != 1 || ms[0].ID != boxID {
		t.Errorf("List u-1 = %+v, want only its tenant's claim", ms)
	}
	if ms, _ := b.List(t.Context(), "u-2"); len(ms) != 1 || ms[0].ID != "cb-b" {
		t.Errorf("List u-2 = %+v, want the claim its tenant stamp names over the owner metadata", ms)
	}
}

func TestTenantsPutKeepsTheTokenAndDeleteIsIdempotent(t *testing.T) {
	fake := newFakeSandboxd(t)
	tenants, err := NewTenants(fake.srv.URL, rootToken, "desk")
	if err != nil {
		t.Fatalf("NewTenants: %v", err)
	}
	if err := tenants.Put(t.Context(), "u-1", "tenant-token", 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tenants.Put(t.Context(), "u-1", "", 2); err != nil {
		t.Fatalf("Put cap: %v", err)
	}
	if got := fake.tenants["u-1"]; got != (tenantRequest{Token: "tenant-token", MaxClaims: 2, EgressClass: "desk"}) {
		t.Errorf("tenant = %+v, want the token kept, the cap changed and the class sent", got)
	}
	for range 2 {
		if err := tenants.Delete(t.Context(), "u-1"); err != nil {
			t.Errorf("Delete: %v", err)
		}
	}
	if len(fake.tenants) != 0 {
		t.Errorf("tenants after Delete = %+v", fake.tenants)
	}
	if raw, _ := json.Marshal(tenantRequest{Token: "t", MaxClaims: 2}); strings.Contains(string(raw), "egress_class") {
		t.Errorf("a classless tenant request sends %s; sandboxd before egress classes refuses the field", raw)
	}
	if _, err := NewTenants("127.0.0.1:17780", rootToken, ""); err == nil {
		t.Error("NewTenants accepted a URL without a scheme")
	}
}

func TestHibernateAndWakeChangeTheIncarnation(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	m := claimRunning(t, b)

	h, err := b.Hibernate(t.Context(), boxID)
	if err != nil || h.State != machines.StateHibernating {
		t.Fatalf("Hibernate = %+v, %v", h, err)
	}
	waitState(t, b, boxID, h.Revision, machines.StateHibernated)
	if _, err := b.Address(t.Context(), boxID); !isNotRunning(err, machines.StateHibernated) {
		t.Errorf("Address of a hibernated box = %v", err)
	}
	w, action, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge})
	if err != nil || action != machines.ActionWoken || w.State != machines.StateStarting {
		t.Fatalf("Ensure of a hibernated box = %+v, %v, %v", w, action, err)
	}
	woken := waitState(t, b, boxID, w.Revision, machines.StateRunning)
	if woken.Incarnation == m.Incarnation || fake.envOf("CB_WAKES") != "1" {
		t.Errorf("incarnation %q -> %q, CB_WAKES %q", m.Incarnation, woken.Incarnation, fake.envOf("CB_WAKES"))
	}
}

func TestARestartOrFailureIsReportedFromTheRow(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	m := claimRunning(t, b)

	fake.mutate(func(sb *fakeSandbox) { sb.row.Restarts++ })
	if got, _ := b.Get(t.Context(), boxID); got.State != machines.StateStarting || got.Incarnation == m.Incarnation {
		t.Errorf("after a restart = %+v", got)
	}
	waitState(t, b, boxID, 0, machines.StateRunning)

	fake.mutate(func(sb *fakeSandbox) { sb.row.Failed = "restart budget exhausted" })
	if got, _ := b.Get(t.Context(), boxID); got.State != machines.StateFailed || got.Reason != "restart budget exhausted" {
		t.Errorf("failed claim = %+v", got)
	}
	w, action, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge})
	if err != nil || action != machines.ActionWoken {
		t.Fatalf("Ensure of a failed box = %+v, %v, %v", w, action, err)
	}
	waitState(t, b, boxID, w.Revision, machines.StateRunning)
}

func TestAddressDialsThePassiveRelay(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)

	ep, err := b.Address(t.Context(), boxID)
	if err != nil || ep.Key != fake.only()+".sandboxd:7443" {
		t.Fatalf("Address = %+v, %v", ep, err)
	}
	conn, err := ep.Dial(t.Context())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("PRI * HTTP/2.0")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len("PRI * HTTP/2.0"))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "PRI * HTTP/2.0" {
		t.Fatalf("relay echo = %q, %v", buf, err)
	}
	if got := fake.lastRelay(); got != "7443 upgrade tcp" {
		t.Errorf("relay request = %q, want port 7443 with the root token and the tcp upgrade", got)
	}
	if _, err := b.Address(t.Context(), "cb-missing"); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("Address of an unknown machine = %v", err)
	}
	fake.mutate(func(sb *fakeSandbox) { sb.row.Hibernated = true })
	if _, err := ep.Dial(t.Context()); !errors.Is(err, machines.ErrConflict) {
		t.Errorf("Dial of a hibernated box = %v, want the relay's 409", err)
	}
}

func TestTheEndpointKeyServesAsAnH2CAuthority(t *testing.T) {
	fake := newFakeSandboxd(t)
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	agent := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	agent.Config.Protocols = &protocols
	agent.Start()
	t.Cleanup(agent.Close)
	fake.agent = agent.Listener.Addr().String()
	b := newTestBackend(t, fake)
	claimRunning(t, b)

	ep, err := b.Address(t.Context(), boxID)
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	transport := &http.Transport{Protocols: &protocols, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return ep.Dial(ctx) }}
	t.Cleanup(transport.CloseIdleConnections)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://agent/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.URL.Host, req.Host = ep.Key, ep.Key
	resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST through the relay: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.ProtoMajor != 2 {
		t.Errorf("POST through the relay = %s %s, want the agent's 401 over HTTP/2", resp.Proto, resp.Status)
	}
}

func TestDeliverInstallsTheGWKeyHostOnlyAndWritesTheIdentity(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	identity := machines.Identity{BoxID: boxID, WakeID: "w1", KeyID: "k1", PublicKey: "pk"}

	if err := b.Deliver(t.Context(), boxID, identity, "sk-box", "door-key"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	for name, want := range map[string]string{"GW_KEY": "Bearer sk-box", "CB_CONNECT_KEY": "Bearer door-key"} {
		if v := fake.env()[name]; v.Value != want || v.Guest == nil || *v.Guest {
			t.Errorf("%s = %+v, want a host-only Bearer entry", name, v)
		}
	}
	if fake.count("patch") != 1 {
		t.Errorf("env patches = %d, want both keys in one", fake.count("patch"))
	}
	exec := fake.lastExec()
	var file machines.Identity
	if err := json.Unmarshal([]byte(exec.Env["CB_STDIN"]), &file); err != nil || file != identity || !slices.Equal(exec.Argv, writeIdentity) {
		t.Errorf("identity exec = %+v (%v)", exec, err)
	}
	if err := b.Deliver(t.Context(), boxID, identity, "", ""); err != nil || fake.count("patch") != 1 {
		t.Errorf("Deliver without a gw key: %v, env patches %d", err, fake.count("patch"))
	}
	if err := b.SetMeta(t.Context(), boxID, "wake", "inc|w1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if v := fake.env()["CB_META_wake"]; v.Value != "inc|w1" || v.Guest != nil || fake.env()["GW_KEY"].Value != "Bearer sk-box" {
		t.Errorf("env after SetMeta = %+v", fake.env())
	}
	if m, _ := b.Get(t.Context(), boxID); m.Meta["wake"] != "inc|w1" {
		t.Errorf("Meta = %v", m.Meta)
	}
	if err := b.SetMeta(t.Context(), boxID, "bad key", "x"); err == nil {
		t.Error("SetMeta accepted a key that is no env name")
	}
}

func TestDeliverFailures(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	identity := machines.Identity{BoxID: boxID, WakeID: "w1"}

	fake.execExit = 1
	if err := b.Deliver(t.Context(), boxID, identity, "", ""); err == nil || !strings.Contains(err.Error(), "exit 1: mkdir failed") {
		t.Errorf("Deliver with a failing exec = %v", err)
	}
	fake.execExit, fake.execStatus = 0, http.StatusBadGateway
	if err := b.Deliver(t.Context(), boxID, identity, "", ""); err == nil || !strings.Contains(err.Error(), "write identity") {
		t.Errorf("Deliver with an unreachable guest = %v", err)
	}
	fake.execStatus = 0
	h, _ := b.Hibernate(t.Context(), boxID)
	waitState(t, b, boxID, h.Revision, machines.StateHibernated)
	execs := fake.count("exec")
	if err := b.Deliver(t.Context(), boxID, identity, "", ""); !isNotRunning(err, machines.StateHibernated) || fake.count("exec") != execs {
		t.Errorf("Deliver to a hibernated box = %v, execs %d -> %d", err, execs, fake.count("exec"))
	}
	if err := b.Deliver(t.Context(), "cb-missing", identity, "", ""); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("Deliver to an unknown machine = %v", err)
	}
}

func TestSetEgressSetsAndRemovesTheHostOnlyUpstream(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	if m := claimRunning(t, b); m.Routed {
		t.Fatalf("new machine is routed: %+v", m)
	}

	if err := b.SetEgress(t.Context(), boxID, "http://10.203.1.2:8823"); err != nil {
		t.Fatalf("SetEgress: %v", err)
	}
	up, ok := fake.env()["EGRESS_UPSTREAM"]
	if !ok || up.Value != "http://10.203.1.2:8823" || up.Guest == nil || *up.Guest {
		t.Errorf("EGRESS_UPSTREAM = %+v, %t, want a host-only entry", up, ok)
	}
	if m, _ := b.Get(t.Context(), boxID); !m.Routed {
		t.Errorf("machine after SetEgress = %+v, want routed", m)
	}
	if err := b.SetEgress(t.Context(), boxID, ""); err != nil {
		t.Fatalf("SetEgress direct: %v", err)
	}
	if _, ok := fake.env()["EGRESS_UPSTREAM"]; ok {
		t.Errorf("env after SetEgress direct = %+v", fake.env())
	}
	if m, _ := b.Get(t.Context(), boxID); m.Routed {
		t.Errorf("machine after SetEgress direct = %+v, want direct", m)
	}
	if err := b.SetEgress(t.Context(), "cb-missing", ""); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("SetEgress of an unknown machine = %v", err)
	}
}

func TestSetCredentialsWritesChangesOnlyAndFlagsOnlyWhileRunning(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	on := machines.Credentials{
		Secrets: map[string]*machines.Secret{
			"GH_GIT":          {Value: "Basic x", Hosts: []string{"github.com"}, Header: "Authorization", Placeholder: "Basic @GH_GIT@"},
			"GOOGLE_CALENDAR": nil,
		},
		Flags: map[string]string{"COCOONBOX_GH_GIT": "1"},
	}
	off := machines.Credentials{
		Secrets: map[string]*machines.Secret{"GH_GIT": nil, "GOOGLE_CALENDAR": nil},
		Flags:   map[string]string{"COCOONBOX_GH_GIT": ""},
	}

	for range 2 {
		if err := b.SetCredentials(t.Context(), boxID, on); err != nil {
			t.Fatalf("SetCredentials: %v", err)
		}
	}
	git, flag := fake.env()["GH_GIT"], fake.env()["COCOONBOX_GH_GIT"]
	if git.Value != "Basic x" || git.Guest == nil || *git.Guest || git.Inject == nil || !slices.Equal(git.Inject.Hosts, []string{"github.com"}) ||
		git.Inject.Header != "Authorization" || git.Inject.Placeholder != "Basic @GH_GIT@" {
		t.Errorf("GH_GIT = %+v (%+v), want a host-only credential for github.com", git, git.Inject)
	}
	if flag.Value != "1" || flag.Guest != nil || fake.count("patch") != 1 {
		t.Errorf("flag = %+v after %d patches, want a guest entry in one patch", flag, fake.count("patch"))
	}
	for range 2 {
		if err := b.SetCredentials(t.Context(), boxID, off); err != nil {
			t.Fatalf("SetCredentials off: %v", err)
		}
	}
	if len(fake.env()) != 0 || fake.count("patch") != 2 {
		t.Errorf("env = %+v after %d patches, want empty after one more", fake.env(), fake.count("patch"))
	}

	h, _ := b.Hibernate(t.Context(), boxID)
	waitState(t, b, boxID, h.Revision, machines.StateHibernated)
	if err := b.SetCredentials(t.Context(), boxID, on); err != nil {
		t.Fatalf("SetCredentials while hibernated: %v", err)
	}
	if _, ok := fake.env()["COCOONBOX_GH_GIT"]; ok || fake.env()["GH_GIT"].Value != "Basic x" {
		t.Errorf("env of a hibernated claim = %+v, want the secret without the flag", fake.env())
	}
}

func TestSetCredentialsRemembersARefusalUntilTheValueChanges(t *testing.T) {
	fake := newFakeSandboxd(t)
	fake.add(&fakeSandbox{
		row: row{ID: "sb_1", Metadata: map[string]string{"cb_machine": boxID, "cb_owner": owner}}, token: "tok",
		env: map[string]envVar{"GH_GIT": {Guest: new(false)}},
	})
	b := newTestBackend(t, fake)
	secret := func(v string) machines.Credentials {
		return machines.Credentials{Secrets: map[string]*machines.Secret{"GH_GIT": {Value: v, Hosts: []string{"github.com"}, Header: "Authorization"}}}
	}

	if err := b.SetCredentials(t.Context(), boxID, machines.Credentials{Secrets: map[string]*machines.Secret{"GH_GIT": nil}}); err != nil {
		t.Fatalf("remove a name the claim held before this process: %v", err)
	}
	if _, ok := fake.env()["GH_GIT"]; ok {
		t.Errorf("env = %+v, want GH_GIT removed", fake.env())
	}
	fake.patchStatus = http.StatusBadRequest
	if err := b.SetCredentials(t.Context(), boxID, secret("Basic x")); !errors.Is(err, machines.ErrRefused) {
		t.Errorf("SetCredentials on a pool that cannot inject = %v, want ErrRefused", err)
	}
	if err := b.SetCredentials(t.Context(), boxID, secret("Basic x")); err != nil || fake.count("patch") != 2 {
		t.Errorf("repeat of the refused value = %v after %d patches, want nil with the value sent once", err, fake.count("patch"))
	}
	fake.patchStatus = 0
	if err := b.SetCredentials(t.Context(), boxID, secret("Basic y")); err != nil || fake.envOf("GH_GIT") != "Basic y" {
		t.Errorf("SetCredentials with a new value = %v, env %+v", err, fake.env())
	}
}

func TestDeleteReleasesTheClaim(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	watch := b.Watch(t.Context(), boxID, 0)
	if m := <-watch; m.State != machines.StateRunning {
		t.Fatalf("first watch state = %+v", m)
	}

	if err := b.Delete(t.Context(), boxID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if fake.count("release") != 1 {
		t.Errorf("releases = %d", fake.count("release"))
	}
	if _, err := b.Get(t.Context(), boxID); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("Get after Delete = %v", err)
	}
	if err := b.Delete(t.Context(), boxID); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("second Delete = %v", err)
	}
	for m := range watch {
		if m.State == machines.StateGone {
			return
		}
	}
	t.Error("watch ended without StateGone")
}

func TestWatchReportsAMetaChange(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	watch := b.Watch(ctx, boxID, 0)
	if m := <-watch; m.State != machines.StateRunning {
		t.Fatalf("first watch state = %+v", m)
	}
	if err := b.SetMeta(t.Context(), boxID, "wake", "inc|w1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	for m := range watch {
		if m.Meta["wake"] == "inc|w1" {
			return
		}
	}
	t.Error("watch sent nothing after the meta changed")
}

func TestRenewExtendsEveryLeaseButArchivedOnes(t *testing.T) {
	fake := newFakeSandboxd(t)
	fake.add(&fakeSandbox{row: row{ID: "sb_a", Metadata: map[string]string{"cb_machine": "cb-a", "cb_owner": owner}}})
	fake.add(&fakeSandbox{row: row{ID: "sb_b", Archived: true, Metadata: map[string]string{"cb_machine": "cb-b", "cb_owner": owner}}})
	b := newTestBackend(t, fake)

	if err := b.renew(t.Context()); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if got := fake.renewed(); !slices.Equal(got, []string{"sb_a 3600 archive"}) {
		t.Errorf("renewed %v", got)
	}
}

type fakeSandbox struct {
	row   row
	token string
	env   map[string]envVar
}

type fakeSandboxd struct {
	srv *httptest.Server

	mu          sync.Mutex
	sandboxes   map[string]*fakeSandbox
	next        int
	calls       map[string]int
	claims      []claimRequest
	execs       []execRequest
	relays      []string
	renews      []string
	execExit    int32
	execStatus  int
	patchStatus int
	agent       string
	tenants     map[string]tenantRequest
}

func (f *fakeSandboxd) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/claim", func(w http.ResponseWriter, r *http.Request) {
		var req claimRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		tenant, ok := f.tokenOwner(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if !ok {
			f.mu.Unlock()
			writeError(w, http.StatusUnauthorized, "bad token")
			return
		}
		f.next++
		sb := &fakeSandbox{row: row{ID: "sb_" + strconv.Itoa(f.next), Key: req.poolKey, Tenant: tenant, Metadata: req.Metadata, ClaimedAt: time.Now().UTC()}, token: "tok", env: map[string]envVar{}}
		f.sandboxes[sb.row.ID] = sb
		f.claims = append(f.claims, req)
		f.calls["claim"]++
		f.mu.Unlock()
		writeJSON(w, map[string]string{"id": sb.row.ID, "token": sb.token})
	})
	mux.HandleFunc("PUT /v1/tenants/{name}", f.root(func(w http.ResponseWriter, r *http.Request) {
		var req tenantRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if req.Token == "" {
			req.Token = f.tenants[r.PathValue("name")].Token
		}
		f.tenants[r.PathValue("name")] = req
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("DELETE /v1/tenants/{name}", f.root(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.tenants[r.PathValue("name")]; !ok {
			writeError(w, http.StatusNotFound, "unknown tenant")
			return
		}
		delete(f.tenants, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /v1/sandboxes", f.root(func(w http.ResponseWriter, r *http.Request) {
		k, v, _ := strings.Cut(r.URL.Query().Get("metadata"), "=")
		f.mu.Lock()
		defer f.mu.Unlock()
		rows := []row{}
		for _, sb := range f.sandboxes {
			if k == "" || sb.row.Metadata[k] == v {
				rows = append(rows, sb.row)
			}
		}
		writeJSON(w, map[string]any{"sandboxes": rows})
	}))
	mux.HandleFunc("GET /v1/sandboxes/{id}", f.sandbox(func(w http.ResponseWriter, _ *http.Request, sb *fakeSandbox) {
		r := sb.row
		r.Token = sb.token
		writeJSON(w, r)
	}))
	mux.HandleFunc("POST /v1/sandboxes/{id}/{verb}", f.sandbox(func(w http.ResponseWriter, r *http.Request, sb *fakeSandbox) {
		f.calls[r.PathValue("verb")]++
		switch r.PathValue("verb") {
		case "wake":
			sb.row.Hibernated, sb.row.Archived, sb.row.Failed = false, false, ""
		case "hibernate":
			sb.row.Hibernated = true
		case "release":
			delete(f.sandboxes, sb.row.ID)
		case "renew":
			var req renewRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if sb.row.Archived {
				writeError(w, http.StatusConflict, "archived")
				return
			}
			f.renews = append(f.renews, fmt.Sprintf("%s %d %s", sb.row.ID, req.TTLSeconds, req.OnExpire))
		case "exec":
			f.exec(w, r, sb)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /v1/sandboxes/{id}/env", f.sandbox(func(w http.ResponseWriter, _ *http.Request, sb *fakeSandbox) {
		blanked := map[string]envVar{}
		for k, v := range sb.env {
			if v.Guest != nil && !*v.Guest {
				v.Value = ""
			}
			blanked[k] = v
		}
		writeJSON(w, map[string]any{"env": blanked})
	}))
	mux.HandleFunc("PATCH /v1/sandboxes/{id}/env", f.sandbox(func(w http.ResponseWriter, r *http.Request, sb *fakeSandbox) {
		var req struct {
			Env map[string]*envVar `json:"env"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.calls["patch"]++
		if f.patchStatus != 0 {
			writeError(w, f.patchStatus, "refused")
			return
		}
		for k, v := range req.Env {
			if v == nil {
				delete(sb.env, k)
			} else {
				sb.env[k] = *v
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /v1/sandboxes/{id}/ports/{port}", f.sandbox(func(w http.ResponseWriter, r *http.Request, sb *fakeSandbox) {
		f.relays = append(f.relays, fmt.Sprintf("%s %s %s", r.PathValue("port"), strings.ToLower(r.Header.Get("Connection")), r.Header.Get("Upgrade")))
		if sb.row.Hibernated || sb.row.Archived || sb.row.Failed != "" {
			writeError(w, http.StatusConflict, "sandbox is paused")
			return
		}
		f.mu.Unlock()
		defer f.mu.Lock()
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		if f.agent == "" {
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, rw)
			}()
			return
		}
		up, err := net.Dial("tcp", f.agent)
		if err != nil {
			_ = conn.Close()
			return
		}
		go func() {
			_, _ = io.Copy(up, rw)
			_ = up.Close()
		}()
		go func() {
			_, _ = io.Copy(conn, up)
			_ = conn.Close()
		}()
	}))
	return mux
}

func (f *fakeSandboxd) exec(w http.ResponseWriter, r *http.Request, sb *fakeSandbox) {
	if r.Header.Get("Authorization") != "Bearer "+sb.token {
		writeError(w, http.StatusNotFound, "unknown sandbox")
		return
	}
	var req execRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.execs = append(f.execs, req)
	if f.execStatus != 0 {
		writeError(w, f.execStatus, "guest unreachable")
		return
	}
	resp := execResponse{ExitCode: f.execExit}
	if f.execExit != 0 {
		resp.Stderr = "mkdir failed"
	}
	writeJSON(w, resp)
}

// root checks the root token; sandbox also resolves {id} and holds f.mu for the handler.
func (f *fakeSandboxd) root(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+rootToken {
			writeError(w, http.StatusUnauthorized, "bad token")
			return
		}
		next(w, r)
	}
}

func (f *fakeSandboxd) sandbox(next func(http.ResponseWriter, *http.Request, *fakeSandbox)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		sb := f.sandboxes[r.PathValue("id")]
		switch {
		case sb == nil:
			writeError(w, http.StatusNotFound, "unknown sandbox")
		case r.PathValue("verb") != "exec" && r.Header.Get("Authorization") != "Bearer "+rootToken:
			writeError(w, http.StatusUnauthorized, "bad token")
		default:
			next(w, r, sb)
		}
	}
}

// tokenOwner returns the tenant a token names, "" for root.
func (f *fakeSandboxd) tokenOwner(token string) (string, bool) {
	if token == rootToken {
		return "", true
	}
	for name, t := range f.tenants {
		if t.Token == token {
			return name, true
		}
	}
	return "", false
}

func (f *fakeSandboxd) add(sb *fakeSandbox) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb.env == nil {
		sb.env = map[string]envVar{}
	}
	f.sandboxes[sb.row.ID] = sb
}

func (f *fakeSandboxd) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, id)
}

// mutate changes the only sandbox of a test that claimed one.
func (f *fakeSandboxd) mutate(fn func(*fakeSandbox)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, sb := range f.sandboxes {
		fn(sb)
	}
}

func (f *fakeSandboxd) only() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.sandboxes {
		return id
	}
	return ""
}

func (f *fakeSandboxd) env() map[string]envVar {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, sb := range f.sandboxes {
		return sb.env
	}
	return nil
}

func (f *fakeSandboxd) envOf(name string) string {
	return f.env()[name].Value
}

func (f *fakeSandboxd) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if call == "exec" {
		return len(f.execs)
	}
	return f.calls[call]
}

func (f *fakeSandboxd) lastClaim() claimRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims[len(f.claims)-1]
}

func (f *fakeSandboxd) lastExec() execRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execs[len(f.execs)-1]
}

func (f *fakeSandboxd) lastRelay() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.relays[len(f.relays)-1]
}

func (f *fakeSandboxd) renewed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.renews)
}

func newFakeSandboxd(t *testing.T) *fakeSandboxd {
	t.Helper()
	f := &fakeSandboxd{sandboxes: map[string]*fakeSandbox{}, calls: map[string]int{}, tenants: map[string]tenantRequest{}}
	f.srv = httptest.NewServer(f.handler())
	t.Cleanup(f.srv.Close)
	return f
}

func newTestBackend(t *testing.T, fake *fakeSandboxd) *Backend {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	b, err := New(ctx, Options{URL: fake.srv.URL, Token: rootToken, Image: template, Flavors: []machines.Flavor{xlarge}, DiskGiB: 40, AgentPort: 7443})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func claimRunning(t *testing.T, b *Backend) machines.Machine {
	t.Helper()
	m, _, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return waitState(t, b, boxID, m.Revision, machines.StateRunning)
}

func waitState(t *testing.T, b *Backend, id string, revision int64, want machines.State) machines.Machine {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var last machines.Machine
	for m := range b.Watch(ctx, id, revision) {
		if last = m; m.State == want {
			return m
		}
	}
	t.Fatalf("machine %s never reached state %d; last %+v", id, want, last)
	return last
}

func isNotRunning(err error, state machines.State) bool {
	var nr *machines.NotRunningError
	return errors.As(err, &nr) && nr.State == state
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
