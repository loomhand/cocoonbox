package sandboxd

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/utils"
)

const (
	rootToken = "root-token"
	boxID     = "cb-0123456789ab"
	owner     = "alice"
	template  = "cbdesk:v8"

	reconnectWithin = 10 * time.Second
	waitFor         = 5 * time.Second
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
	if !reflect.DeepEqual(claim, want) {
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

func TestAClaimWhoseReadFailsIsKeptAndNotClaimedAgain(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	fake.mu.Lock()
	fake.getStatus = http.StatusInternalServerError
	fake.mu.Unlock()
	m, _, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	clustertest.WaitFor(t, "the claim ends", func() bool {
		e, err := b.stored(t.Context(), boxID)
		return err == nil && !e.busy(time.Now())
	})
	fake.mu.Lock()
	fake.getStatus = 0
	fake.mu.Unlock()
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, _, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge}); err != nil {
		t.Fatalf("Ensure after the failed read: %v", err)
	}
	if got := waitState(t, b, boxID, m.Revision, machines.StateRunning); fake.count("claim") != 1 || !strings.Contains(got.Detail, "sandbox=sb_1 ") {
		t.Errorf("machine %s after %d claims, want sb_1 running from the one claim", got.Detail, fake.count("claim"))
	}
}

func TestAListingNeitherCreatesNorMovesNorReownsAMachine(t *testing.T) {
	entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, peer.peers = []*fakeSandboxd{peer}, []*fakeSandboxd{entry}
	entry.add(&fakeSandbox{row: row{ID: "sb_orphan", Key: poolKey{Template: template, Size: "xlarge"}, Metadata: map[string]string{"cb_machine": "cb-orphan", "cb_owner": owner}}})
	b := newTestBackend(t, entry)
	m := claimRunning(t, b)
	peer.add(&fakeSandbox{row: row{ID: "sb_evil", Key: poolKey{Template: template, Size: "xlarge"}, Tenant: "mallory", Metadata: map[string]string{"cb_machine": boxID, "cb_owner": "mallory"}}, token: "evil"})

	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, err := b.Get(t.Context(), boxID)
	if err != nil || got.Owner != owner || got.Node != m.Node || got.Detail != m.Detail {
		t.Errorf("machine after a foreign listing = %+v, %v; want it unchanged on %s", got, err, m.Node)
	}
	if ms, _ := b.List(t.Context(), ""); len(ms) != 1 {
		t.Errorf("List = %+v, want only the machine the backend claimed", ms)
	}
	if _, err := b.Get(t.Context(), "cb-orphan"); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("Get of a listed claim the backend never made = %v, want ErrNotFound", err)
	}
}

func TestAClaimShowsAndClearsItsMissingEnv(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	secret := machines.Credentials{Secrets: map[string]*machines.Secret{"GH_GIT": {Value: "Basic x", Hosts: []string{"github.com"}, Header: "Authorization"}}}
	if err := b.SetCredentials(t.Context(), boxID, secret); err != nil {
		t.Fatalf("SetCredentials: %v", err)
	}
	fake.lose("sb_1", "GH_GIT")

	m, err := b.Get(t.Context(), boxID)
	if err != nil || !m.EnvMissing {
		t.Fatalf("Get after the node lost a value = %+v, %v; want EnvMissing", m, err)
	}
	if err := b.SetCredentials(t.Context(), boxID, secret); err != nil || fake.envOf("GH_GIT") != "Basic x" {
		t.Fatalf("push of the unchanged value = %v, env %+v; want it written again", err, fake.env())
	}
	if in := fake.env()["GH_GIT"].Inject; in == nil || !slices.Equal(in.Hosts, []string{"github.com"}) || in.Header != "Authorization" {
		t.Errorf("inject after the push = %+v, want the whole entry", in)
	}
	if m, err := b.Get(t.Context(), boxID); err != nil || m.EnvMissing {
		t.Errorf("Get after the push = %+v, %v; want EnvMissing cleared", m, err)
	}
}

func TestAnEmptyHostOnlyValueGoesAsARemoval(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	if err := b.SetCredentials(t.Context(), boxID, machines.Credentials{Secrets: map[string]*machines.Secret{"GH_GIT": {Value: "Basic x", Hosts: []string{"github.com"}}}}); err != nil {
		t.Fatalf("SetCredentials: %v", err)
	}
	if err := b.SetCredentials(t.Context(), boxID, machines.Credentials{Secrets: map[string]*machines.Secret{"GH_GIT": {Hosts: []string{"github.com"}}}}); err != nil {
		t.Fatalf("SetCredentials of an empty value: %v", err)
	}
	if _, ok := fake.env()["GH_GIT"]; ok {
		t.Errorf("env = %+v, want GH_GIT removed", fake.env())
	}
	if err := b.SetCredentials(t.Context(), boxID, machines.Credentials{Secrets: map[string]*machines.Secret{"GW_KEY": {}}}); err != nil {
		t.Errorf("SetCredentials of an empty key: %v", err)
	}
}

func TestAnAPIWithNodesCallsNoOtherNode(t *testing.T) {
	entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, peer.peers = []*fakeSandboxd{peer}, []*fakeSandboxd{entry}
	entry.redirect = []*fakeSandboxd{peer}
	b, err := New(t.Context(), Options{URL: entry.srv.URL, Token: rootToken, Image: template, Flavors: []machines.Flavor{xlarge}, DiskGiB: 40, AgentPort: 7443, Store: clustertest.Open(t), Nodes: []string{"10.0.0.9:7777"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go b.Run(t.Context())
	m := claimRunning(t, b)
	if peer.count("claim") != 0 || entry.count("claim") != 1 || m.Node != entry.addr() {
		t.Errorf("claims on the peer %d, the entry %d, node %s; want the entry's only", peer.count("claim"), entry.count("claim"), m.Node)
	}
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if n := peer.count("list"); n != 0 {
		t.Errorf("lists on the peer = %d, want none", n)
	}
}

func TestDropMissingRemovesWhatTheNodeLostAndNothingRefills(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	if err := b.SetCredentials(t.Context(), boxID, machines.Credentials{Secrets: map[string]*machines.Secret{"GH_GIT": {Value: "Basic x", Hosts: []string{"github.com"}}, "CBV_1": {Value: "v"}}}); err != nil {
		t.Fatalf("SetCredentials: %v", err)
	}
	if err := b.SetEgress(t.Context(), boxID, "http://proxy:3128"); err != nil {
		t.Fatalf("SetEgress: %v", err)
	}
	fake.lose("sb_1", "GH_GIT")
	fake.lose("sb_1", "EGRESS_UPSTREAM")

	dropped, err := b.DropMissing(t.Context(), boxID, []string{"CBV_1"})
	if err != nil || !slices.Equal(dropped, []string{"EGRESS_UPSTREAM", "GH_GIT"}) {
		t.Fatalf("DropMissing = %v, %v", dropped, err)
	}
	if env := fake.env(); len(env) != 1 || env["CBV_1"].Value != "v" {
		t.Errorf("env = %+v, want only the entry the node kept", env)
	}
	if m, err := b.Get(t.Context(), boxID); err != nil || m.EnvMissing || m.Routed {
		t.Errorf("Get = %+v, %v; want nothing missing and no upstream", m, err)
	}
}

func TestAListedRowWithoutAnIDChangesNothing(t *testing.T) {
	fake := newFakeSandboxd(t)
	fake.add(&fakeSandbox{row: row{Key: poolKey{Template: template}}})
	b := newTestBackend(t, fake)
	m := claimRunning(t, b)
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got, err := b.Get(t.Context(), boxID); err != nil || got.Node != m.Node {
		t.Errorf("Get = %+v, %v", got, err)
	}
}

func TestOwnerAnswersThatDisagreeOrABlipMoveNothing(t *testing.T) {
	entry, peer, evil := newFakeSandboxd(t), newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, entry.redirect = []*fakeSandboxd{peer, evil}, []*fakeSandboxd{peer}
	b := newTestBackend(t, entry)
	claimRunning(t, b)
	peer.mu.Lock()
	sb := peer.sandboxes["sb_1"]
	peer.mu.Unlock()
	evil.add(&fakeSandbox{row: sb.row, token: sb.token})
	peer.shift(entry, entry.adoptable)

	if _, err := b.Get(t.Context(), boxID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e, err := b.stored(t.Context(), boxID); err != nil || e.ownerAddr != peer.addr() {
		t.Errorf("owner = %+v, %v; want the machine kept on %s while nodes disagree", e, err, peer.addr())
	}
	evil.remove("sb_1")
	peer.srv.Close()
	asked := entry.count("owner")
	if _, err := b.Get(t.Context(), boxID); err == nil {
		t.Error("Get through a node that does not answer succeeded")
	}
	if entry.count("owner") != asked {
		t.Error("a node that does not answer but stays in the mesh made the backend follow")
	}
}

func TestNodesRefuseAnEntryPosingAsANodeAndAMachineOnANodeLeftOut(t *testing.T) {
	entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, entry.redirect = []*fakeSandboxd{peer}, []*fakeSandboxd{peer}
	db := clustertest.Open(t)
	opts := Options{URL: entry.srv.URL, Token: rootToken, Image: template, Flavors: []machines.Flavor{xlarge}, DiskGiB: 40, AgentPort: 7443, Store: db, Nodes: []string{entry.addr()}}
	if _, err := New(t.Context(), opts); err == nil {
		t.Error("New accepted an entry node that advertises an address nodes lists")
	}
	claimRunning(t, newSharedBackend(t, entry, db))
	opts.Nodes = []string{"10.0.0.9:7777"}
	if _, err := New(t.Context(), opts); err == nil {
		t.Error("New accepted a machine on a node nodes leaves out")
	}
}

func TestARereadRefusesAnotherMachinesSandbox(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	fake.add(&fakeSandbox{row: row{ID: "sb_bob", Metadata: map[string]string{"cb_machine": "cb-bob", "cb_owner": "bob"}}})
	if err := b.reread(t.Context(), boxID, "", fake.addr(), "sb_bob"); err == nil {
		t.Error("reread bound a machine to another machine's sandbox")
	}
}

func TestAMovedClaimWaitsForAnAPIThatHoldsItsTokenAndAListingLendsNone(t *testing.T) {
	entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, entry.redirect = []*fakeSandboxd{peer}, []*fakeSandboxd{peer}
	db := clustertest.Open(t)
	holder := openSharedBackend(t, entry, db)
	claimRunning(t, holder)
	token := holder.token(boxID, "sb_1")
	peer.shift(entry, entry.sandboxes)
	entry.mutate(func(sb *fakeSandbox) { sb.row.Token = "forged" })

	fresh := openSharedBackend(t, entry, db)
	if e, err := fresh.stored(t.Context(), boxID); err != nil || e.ownerAddr != peer.addr() || fresh.token(boxID, "sb_1") != "" {
		t.Fatalf("a fresh api's machine = %+v, %v, token %q; want it kept on %s with no token from the listing", e, err, fresh.token(boxID, "sb_1"), peer.addr())
	}
	if err := holder.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if e, err := holder.stored(t.Context(), boxID); err != nil || e.ownerAddr != entry.addr() || holder.token(boxID, "sb_1") != token {
		t.Errorf("after the token holder's refresh = %+v, %v; want it followed to %s with its own token", e, err, entry.addr())
	}
}

func TestAStaleAnswerFromTheFormerNodeLeavesAMovedMachineMoved(t *testing.T) {
	front, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	front.peers, front.redirect = []*fakeSandboxd{peer}, []*fakeSandboxd{peer}
	b := openSharedBackend(t, front, clustertest.Open(t))
	claimRunning(t, b)
	stale, err := b.stored(t.Context(), boxID)
	if err != nil {
		t.Fatalf("stored: %v", err)
	}
	if _, err := b.mutate(t.Context(), boxID, func(e *entry) (*entry, bool) { e.ownerAddr = front.addr(); return e, true }); err != nil {
		t.Fatalf("move: %v", err)
	}
	token := b.token(boxID, "sb_1")
	forged := *stale.row
	forged.EnvMissing, forged.Token = true, "forged"
	if err := b.index(t.Context(), peer.addr(), []row{forged}, map[string]*entry{boxID: stale}, stale.ownerAddr); err != nil {
		t.Fatalf("index: %v", err)
	}
	peer.mutate(func(sb *fakeSandbox) { sb.token = "forged" })
	if err := b.reread(t.Context(), boxID, peer.addr(), peer.addr(), "sb_1"); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if e, err := b.stored(t.Context(), boxID); err != nil || e.ownerAddr != front.addr() || e.row.EnvMissing || b.token(boxID, "sb_1") != token {
		t.Errorf("machine after stale answers from %s = %+v, %v, token %q; want it left on %s with its token", peer.addr(), e, err, b.token(boxID, "sb_1"), front.addr())
	}
	if err := b.dropUnlisted(t.Context(), map[string][]row{peer.addr(): {}}, map[string]*entry{boxID: stale}); err != nil {
		t.Fatalf("dropUnlisted: %v", err)
	}
	if _, err := b.stored(t.Context(), boxID); err != nil {
		t.Errorf("a refresh that saw the former placement deleted the moved machine: %v", err)
	}
}

func TestARefreshKeepsAMachineWhoseNewNodesDisagree(t *testing.T) {
	entry, peer, evil := newFakeSandboxd(t), newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, entry.redirect = []*fakeSandboxd{peer, evil}, []*fakeSandboxd{peer}
	b := newTestBackend(t, entry)
	claimRunning(t, b)
	peer.mu.Lock()
	sb := peer.sandboxes["sb_1"]
	peer.mu.Unlock()
	evil.mu.Lock()
	evil.adoptable["sb_1"] = &fakeSandbox{row: sb.row, token: sb.token}
	evil.mu.Unlock()
	peer.shift(entry, entry.sandboxes)

	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if e, err := b.stored(t.Context(), boxID); err != nil || e.ownerAddr != peer.addr() {
		t.Errorf("machine = %+v, %v; want it kept on %s while its new nodes disagree", e, err, peer.addr())
	}
}

func TestSetCredentialsRefusesANodeOtherThanTheRecordedOne(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	secret := map[string]*machines.Secret{"GW_KEY": {Value: "Bearer k"}}
	if err := b.SetCredentials(t.Context(), boxID, machines.Credentials{Secrets: secret, Node: "10.0.0.9:7777"}); !errors.Is(err, machines.ErrConflict) || fake.count("patch") != 0 {
		t.Errorf("SetCredentials for another node = %v after %d patches, want ErrConflict and none", err, fake.count("patch"))
	}
	if err := b.SetCredentials(t.Context(), boxID, machines.Credentials{Secrets: secret, Node: fake.addr()}); err != nil || fake.envOf("GW_KEY") != "Bearer k" {
		t.Errorf("SetCredentials for its node = %v", err)
	}
}

func TestDropMissingKeepsAnEntryTheCallerStillPushes(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	if err := b.SetCredentials(t.Context(), boxID, machines.Credentials{Secrets: map[string]*machines.Secret{"GW_KEY": {Value: "Bearer k"}}}); err != nil {
		t.Fatalf("SetCredentials: %v", err)
	}
	fake.lose("sb_1", "GW_KEY")
	if dropped, err := b.DropMissing(t.Context(), boxID, []string{"GW_KEY"}); !errors.Is(err, machines.ErrConflict) || len(dropped) != 0 {
		t.Errorf("DropMissing = %v, %v; want ErrConflict and nothing dropped", dropped, err)
	}
	if _, ok := fake.env()["GW_KEY"]; !ok {
		t.Error("the entry the caller still pushes is gone")
	}
}

func TestTheClientFollowsNoHTTPRedirect(t *testing.T) {
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	t.Cleanup(elsewhere.Close)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(node.Close)
	c, err := newClient(node.URL, rootToken)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if err := c.patchEnv(t.Context(), "sb_1", map[string]*envVar{"GW_KEY": {Value: "Bearer k", Guest: new(false)}}); err == nil || reached.Load() != 0 {
		t.Errorf("patchEnv through a redirect = %v, reached the other server %d times; want an error and none", err, reached.Load())
	}
}

func TestAClaimCarriesItsMetaFromCreationThroughARestart(t *testing.T) {
	fake := newFakeSandboxd(t)
	db := clustertest.Open(t)
	b := newSharedBackend(t, fake, db)
	m, _, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge, Meta: map[string]string{"restore": "s1"}})
	if err != nil || m.Meta["restore"] != "s1" {
		t.Fatalf("Ensure = %+v, %v; want the meta before the claim lands", m, err)
	}
	waitState(t, b, boxID, m.Revision, machines.StateRunning)
	if claim := fake.lastClaim(); claim.Metadata["cb_meta_restore"] != "s1" {
		t.Errorf("claim metadata = %v", claim.Metadata)
	}
	if err := b.SetMeta(t.Context(), boxID, "restored", "s1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	got, err := newSharedBackend(t, fake, db).Get(t.Context(), boxID)
	if err != nil || got.Meta["restore"] != "s1" || got.Meta["restored"] != "s1" {
		t.Errorf("meta after a restart = %v, %v", got.Meta, err)
	}
}

func TestATenantClaimBelongsToItsTenant(t *testing.T) {
	fake := newFakeSandboxd(t)
	tenants, err := NewTenants(fake.srv.URL, rootToken, "desk")
	if err != nil {
		t.Fatalf("NewTenants: %v", err)
	}
	if err := tenants.PutTenant(t.Context(), "u-1", "tenant-token", 1); err != nil {
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
	if ms, _ := b.List(t.Context(), "u-2"); len(ms) != 0 {
		t.Errorf("List u-2 = %+v, want no machine from a listing", ms)
	}
}

func TestTenantsPutKeepsTheTokenAndDeleteIsIdempotent(t *testing.T) {
	fake := newFakeSandboxd(t)
	tenants, err := NewTenants(fake.srv.URL, rootToken, "desk")
	if err != nil {
		t.Fatalf("NewTenants: %v", err)
	}
	if err := tenants.PutTenant(t.Context(), "u-1", "tenant-token", 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tenants.PutTenant(t.Context(), "u-1", "", 2); err != nil {
		t.Fatalf("Put cap: %v", err)
	}
	if got := fake.tenants["u-1"]; got != (tenantRequest{Token: "tenant-token", MaxClaims: 2, EgressClass: "desk"}) {
		t.Errorf("tenant = %+v, want the token kept, the cap changed and the class sent", got)
	}
	for range 2 {
		if err := tenants.DeleteTenant(t.Context(), "u-1"); err != nil {
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

func TestTheBackendManagesTenantsWithItsOwnClientAndClass(t *testing.T) {
	fake := newFakeSandboxd(t)
	b, err := New(t.Context(), Options{URL: fake.srv.URL, Token: rootToken, Image: template, Flavors: []machines.Flavor{xlarge}, DiskGiB: 40, AgentPort: 7443, TenantEgressClass: "desk", Store: clustertest.Open(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var tenants machines.Tenants = b
	if err := tenants.PutTenant(t.Context(), "u-1", "tenant-token", 3); err != nil {
		t.Fatalf("PutTenant: %v", err)
	}
	if got := fake.tenants["u-1"]; got != (tenantRequest{Token: "tenant-token", MaxClaims: 3, EgressClass: "desk"}) {
		t.Errorf("tenant = %+v, want the backend's egress class", got)
	}
	if err := tenants.DeleteTenant(t.Context(), "u-1"); err != nil || len(fake.tenants) != 0 {
		t.Errorf("DeleteTenant = %v, tenants %+v", err, fake.tenants)
	}
}

func TestHibernateDuringAClaimOrWakeIsAConflict(t *testing.T) {
	for _, op := range []string{opClaim, opWake} {
		t.Run(op, func(t *testing.T) {
			fake := newFakeSandboxd(t)
			b := newTestBackend(t, fake)
			if op == opWake {
				claimRunning(t, b)
			}
			if _, err := b.mutate(t.Context(), boxID, func(e *entry) (*entry, bool) {
				e = cmp.Or(e, &entry{id: boxID, owner: owner})
				e.begin(op, time.Now(), b.leaseFor)
				return e, true
			}); err != nil {
				t.Fatalf("begin a %s: %v", op, err)
			}
			if _, err := b.Hibernate(t.Context(), boxID); !errors.Is(err, machines.ErrConflict) || fake.count("hibernate") != 0 {
				t.Errorf("Hibernate during a %s = %v after %d hibernates, want ErrConflict and none", op, err, fake.count("hibernate"))
			}
		})
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
	if woken.Incarnation == m.Incarnation {
		t.Errorf("incarnation %q stayed after a wake", woken.Incarnation)
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
	if err != nil || ep.Key != fake.only()+"."+utils.HashHex(b.entryAddr)[:32]+".sandboxd:7443" {
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
	screen, err := ep.Screen(t.Context())
	if err != nil {
		t.Fatalf("Screen: %v", err)
	}
	defer screen.Close()
	if got := fake.lastRelay(); got != "5900 upgrade tcp" {
		t.Errorf("screen relay request = %q, want port 5900 with the tcp upgrade", got)
	}
	if _, err := screen.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatalf("screen write: %v", err)
	}
	if err := screen.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if got, err := io.ReadAll(screen); err != nil || string(got) != "RFB 003.008\n" {
		t.Errorf("screen echo after half-close = %q, %v", got, err)
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

func TestSetCredentialsInstallsTheKeysHostOnlyAndDeliverWritesTheIdentity(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	identity := machines.Identity{BoxID: boxID, WakeID: "w1", KeyID: "k1", PublicKey: "pk", Name: "desk_1"}

	if err := b.SetCredentials(t.Context(), boxID, machines.Credentials{Secrets: map[string]*machines.Secret{"GW_KEY": {Value: "Bearer sk-box"}, "CB_CONNECT_KEY": {Value: "Bearer door-key"}}}); err != nil {
		t.Fatalf("SetCredentials: %v", err)
	}
	if err := b.Deliver(t.Context(), boxID, identity); err != nil {
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
	if err := b.Deliver(t.Context(), boxID, identity); err != nil || fake.count("patch") != 1 {
		t.Errorf("Deliver without a gw key: %v, env patches %d", err, fake.count("patch"))
	}
	if err := b.SetMeta(t.Context(), boxID, "wake", "inc|w1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if _, ok := fake.env()["CB_META_wake"]; ok || fake.count("patch") != 1 {
		t.Errorf("env after SetMeta = %+v, want the meta kept off the claim", fake.env())
	}
	if m, _ := b.Get(t.Context(), boxID); m.Meta["wake"] != "inc|w1" {
		t.Errorf("Meta = %v", m.Meta)
	}
}

func TestDeliverReportsAFailedExecAnUnreachableGuestAHibernatedBoxAndAnUnknownMachine(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	identity := machines.Identity{BoxID: boxID, WakeID: "w1"}

	fake.execExit = 1
	if err := b.Deliver(t.Context(), boxID, identity); err == nil || !strings.Contains(err.Error(), "exit 1: mkdir failed") {
		t.Errorf("Deliver with a failing exec = %v", err)
	}
	fake.execExit, fake.execStatus = 0, http.StatusBadGateway
	if err := b.Deliver(t.Context(), boxID, identity); err == nil || !strings.Contains(err.Error(), "write identity") {
		t.Errorf("Deliver with an unreachable guest = %v", err)
	}
	fake.execStatus = 0
	h, _ := b.Hibernate(t.Context(), boxID)
	waitState(t, b, boxID, h.Revision, machines.StateHibernated)
	execs := fake.count("exec")
	if err := b.Deliver(t.Context(), boxID, identity); !isNotRunning(err, machines.StateHibernated) || fake.count("exec") != execs {
		t.Errorf("Deliver to a hibernated box = %v, execs %d -> %d", err, execs, fake.count("exec"))
	}
	if err := b.Deliver(t.Context(), "cb-missing", identity); !errors.Is(err, machines.ErrNotFound) {
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
	b := newTestBackend(t, fake)
	seed(t, b, fake, &fakeSandbox{
		row: row{ID: "sb_1", Metadata: map[string]string{"cb_machine": boxID, "cb_owner": owner}}, token: "tok",
		env: map[string]envVar{"GH_GIT": {Guest: new(false)}},
	})
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

func TestSetCredentialsPrunesNamesUnderThePrefixThatSecretsLacks(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	seed(t, b, fake, &fakeSandbox{
		row: row{ID: "sb_1", Metadata: map[string]string{"cb_machine": boxID, "cb_owner": owner}}, token: "tok",
		env: map[string]envVar{"CBV_old": {Guest: new(false)}, "GH_GIT": {Guest: new(false)}},
	})
	c := machines.Credentials{
		Secrets: map[string]*machines.Secret{"CBV_new": {Value: "v", Hosts: []string{"serpapi.com"}, Query: "api_key", Placeholder: "ph"}},
		Prune:   "CBV_",
	}

	if err := b.SetCredentials(t.Context(), boxID, c); err != nil {
		t.Fatalf("SetCredentials: %v", err)
	}
	env := fake.env()
	if _, ok := env["CBV_old"]; ok {
		t.Errorf("env = %+v, want CBV_old pruned", env)
	}
	if _, ok := env["GH_GIT"]; !ok {
		t.Errorf("env = %+v, want GH_GIT kept outside the prefix", env)
	}
	if in := env["CBV_new"].Inject; in == nil || in.Query != "api_key" || in.Header != "" || in.Placeholder != "ph" {
		t.Errorf("CBV_new inject = %+v, want the query parameter api_key", in)
	}
	if err := b.SetCredentials(t.Context(), boxID, c); err != nil || fake.count("patch") != 1 {
		t.Errorf("repeat = %v after %d patches, want no second patch", err, fake.count("patch"))
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

func TestARetryOfARefusedClaimKeepsItsImage(t *testing.T) {
	fake := newFakeSandboxd(t)
	fake.refusals = 1
	b := newTestBackend(t, fake)
	m, _, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge, Image: "cbcore:v1"})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	waitState(t, b, boxID, m.Revision, machines.StateFailed)
	if _, _, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge}); err != nil {
		t.Fatalf("Ensure again: %v", err)
	}
	if got := waitState(t, b, boxID, m.Revision, machines.StateRunning); got.Image != "cbcore:v1" || fake.lastClaim().Template != "cbcore:v1" {
		t.Errorf("retried claim of %s, machine image %s; want cbcore:v1", fake.lastClaim().Template, got.Image)
	}
}

func TestAClaimRefusedForCapacityFailsAndDeleteForgetsIt(t *testing.T) {
	fake := newFakeSandboxd(t)
	fake.refusals = 1
	b := newTestBackend(t, fake)
	m, _, err := b.Ensure(t.Context(), machines.Spec{ID: boxID + "-r1", Owner: owner, Flavor: xlarge})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if failed := waitState(t, b, m.ID, m.Revision, machines.StateFailed); failed.Reason != machines.ErrCapacity.Error() {
		t.Errorf("refused claim reason = %q, want %q", failed.Reason, machines.ErrCapacity)
	}

	if err := b.Delete(t.Context(), m.ID); err != nil {
		t.Errorf("Delete of the refused claim = %v, want nil", err)
	}
	if ms, _ := b.List(t.Context(), owner); len(ms) != 0 {
		t.Errorf("List after Delete = %+v, want no machine", ms)
	}
	if fake.count("release") != 0 {
		t.Errorf("releases = %d, want 0 for a claim that never existed", fake.count("release"))
	}
	if got := claimRunning(t, b); got.ID != boxID {
		t.Errorf("claim after the refusal = %+v", got)
	}
}

func TestDeleteRefusesAMachineWhoseClaimIsInFlight(t *testing.T) {
	fake := newFakeSandboxd(t)
	fake.hold = make(chan struct{})
	b := newTestBackend(t, fake)
	m, _, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := b.Delete(t.Context(), boxID); !errors.Is(err, machines.ErrConflict) {
		t.Errorf("Delete during the claim = %v, want ErrConflict", err)
	}
	close(fake.hold)
	waitState(t, b, boxID, m.Revision, machines.StateRunning)
	if err := b.Delete(t.Context(), boxID); err != nil || fake.count("release") != 1 {
		t.Errorf("Delete after the claim = %v with %d releases, want nil and 1", err, fake.count("release"))
	}
}

func TestWatchReportsAMetaChange(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	ctx, cancel := context.WithTimeout(t.Context(), waitFor)
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

func TestWatchSendsAnOpEndAndAMetaChangeBeforeTheNextPoll(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	ctx, cancel := context.WithTimeout(t.Context(), waitFor)
	defer cancel()
	watch := b.Watch(ctx, boxID, 0)
	if m := <-watch; m.State != machines.StateRunning {
		t.Fatalf("first watch state = %+v", m)
	}
	start := time.Now()
	if _, err := b.Hibernate(t.Context(), boxID); err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	m := <-watch
	for m.State == machines.StateHibernating {
		m = <-watch
	}
	if m.State != machines.StateHibernated {
		t.Fatalf("watch state after Hibernate = %+v", m)
	}
	if err := b.SetMeta(t.Context(), boxID, "wake", "inc|w1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if m := <-watch; m.Meta["wake"] != "inc|w1" {
		t.Fatalf("watch after SetMeta = %+v", m)
	}
	if took := time.Since(start); took >= watchInterval/2 {
		t.Errorf("watch took %s to send the op end and the meta change, want under %s", took, watchInterval/2)
	}
}

func TestChangesWakesTheOwnerWhenARefreshSeesAChangeOrARemoval(t *testing.T) {
	fake := newFakeSandboxd(t)
	db, dsn := clustertest.OpenDSN(t)
	b := newSharedBackend(t, fake, db)
	claimRunning(t, b)
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	fenceListener(t, b, dsn)
	mine, others := b.Changes(owner), b.Changes("bob")
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if isClosed(mine) {
		t.Error("a refresh that saw no change woke the owner")
	}
	fake.mutate(func(sb *fakeSandbox) { sb.row.Hibernated = true })
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !isClosed(mine) || isClosed(others) {
		t.Errorf("after a sandboxd-side hibernation: owner woken %t, bob woken %t", isClosed(mine), isClosed(others))
	}
	mine = b.Changes(owner)
	fake.remove(fake.only())
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !isClosed(mine) || isClosed(others) {
		t.Errorf("after a removal: owner woken %t, bob woken %t", isClosed(mine), isClosed(others))
	}
}

func TestRenewExtendsEveryLeaseButArchivedOnes(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	seed(t, b, fake, &fakeSandbox{row: row{ID: "sb_a", Metadata: map[string]string{"cb_machine": "cb-a", "cb_owner": owner}}})
	seed(t, b, fake, &fakeSandbox{row: row{ID: "sb_b", Archived: true, Metadata: map[string]string{"cb_machine": "cb-b", "cb_owner": owner}}})

	if err := b.renew(t.Context()); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if got := fake.renewed(); !slices.Equal(got, []string{"sb_a 3600 archive"}) {
		t.Errorf("renewed %v", got)
	}
}

func TestTwoBackendsOnASharedTableClaimOnceAndSeeEachOthersChanges(t *testing.T) {
	fake := newFakeSandboxd(t)
	db := clustertest.Open(t)
	a, b := newSharedBackend(t, fake, db), newSharedBackend(t, fake, db)
	var wg sync.WaitGroup
	for _, be := range []*Backend{a, b} {
		wg.Go(func() {
			if _, _, err := be.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge}); err != nil {
				t.Errorf("Ensure: %v", err)
			}
		})
	}
	wg.Wait()
	waitState(t, a, boxID, 1, machines.StateRunning)
	waitState(t, b, boxID, 1, machines.StateRunning)
	if n := fake.count("claim"); n != 1 {
		t.Fatalf("claims = %d, want one for two concurrent Ensures", n)
	}
	if err := b.SetMeta(t.Context(), boxID, "slept", "7"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if m, err := rowOf(t, a, boxID); err != nil || m.Meta["slept"] != "7" {
		t.Fatalf("a reads %+v, %v, want b's meta", m, err)
	}
	m, err := a.Hibernate(t.Context(), boxID)
	if err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	waitState(t, b, boxID, m.Revision, machines.StateHibernated)
	if _, action, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge}); err != nil || action != machines.ActionWoken {
		t.Fatalf("Ensure of the hibernated machine on b = %v, %v", action, err)
	}
	running := waitState(t, a, boxID, m.Revision+1, machines.StateRunning)
	c := newSharedBackend(t, fake, db)
	got, err := rowOf(t, c, boxID)
	if err != nil || got.Incarnation != running.Incarnation || got.Meta["slept"] != "7" || got.Revision != running.Revision {
		t.Errorf("a third backend reads %+v, %v, want %+v", got, err, running)
	}
}

func TestAKeyOneBackendWroteIsPrunedByTheOther(t *testing.T) {
	fake := newFakeSandboxd(t)
	db := clustertest.Open(t)
	a, b := newSharedBackend(t, fake, db), newSharedBackend(t, fake, db)
	claimRunning(t, a)
	key := &machines.Secret{Value: "k1", Hosts: []string{"api.example.com"}, Header: "Authorization"}
	if err := a.SetCredentials(t.Context(), boxID, machines.Credentials{Secrets: map[string]*machines.Secret{"CBV_k": key}, Prune: "CBV_"}); err != nil {
		t.Fatalf("SetCredentials on a: %v", err)
	}
	if err := b.SetCredentials(t.Context(), boxID, machines.Credentials{Prune: "CBV_"}); err != nil {
		t.Fatalf("SetCredentials on b: %v", err)
	}
	if _, ok := fake.env()["CBV_k"]; ok {
		t.Error("the key a wrote stayed after b pruned the vault names")
	}
}

func TestAWatchOnAnotherBackendSeesAMachineRecreatedUnderItsID(t *testing.T) {
	fake := newFakeSandboxd(t)
	db := clustertest.Open(t)
	a, b := newSharedBackend(t, fake, db), newSharedBackend(t, fake, db)
	for range 3 {
		m := claimRunning(t, a)
		waitState(t, b, boxID, m.Revision, machines.StateRunning)
		if err := a.Delete(t.Context(), boxID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	m := claimRunning(t, b)
	if got := waitState(t, a, boxID, m.Revision, machines.StateRunning); got.Revision != 1 {
		t.Errorf("a sees revision %d of the recreated machine, want 1", got.Revision)
	}
}

func TestAnOpWhoseLeaseCannotBeRenewedStopsBeforeTheLeaseRunsOut(t *testing.T) {
	fake := newFakeSandboxd(t)
	db, err := cluster.Open(t.Context(), clustertest.EmptyDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	b := newSharedBackend(t, fake, db)
	b.leaseFor, b.renewEvery = 400*time.Millisecond, 20*time.Millisecond
	m := claimRunning(t, b)
	if _, err := db.UpdateMachine(t.Context(), boxID, func(r *cluster.Machine, _ bool) (bool, error) {
		r.Op, r.OpUntil = opHibernate, time.Now().Add(b.leaseFor)
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateMachine: %v", err)
	}
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	go b.renewOp(ctx, stop, boxID, opHibernate, m.Revision)
	time.Sleep(2 * b.leaseFor)
	if ctx.Err() != nil {
		t.Fatal("an op whose lease renews was stopped")
	}
	db.Close()
	start := time.Now()
	select {
	case <-ctx.Done():
		if waited := time.Since(start); waited >= b.leaseFor {
			t.Errorf("the op stopped %v after the last renewal, past its %v lease", waited, b.leaseFor)
		}
	case <-time.After(waitFor):
		t.Fatal("an op whose lease could not be renewed kept running")
	}
}

func TestAnOpOfAGoneProcessEndsAtItsDeadline(t *testing.T) {
	fake := newFakeSandboxd(t)
	db := clustertest.Open(t)
	a := newSharedBackend(t, fake, db)
	claimRunning(t, a)
	if _, err := db.UpdateMachine(t.Context(), boxID, func(m *cluster.Machine, _ bool) (bool, error) {
		m.Op, m.OpUntil, m.Revision = opHibernate, time.Now().Add(-time.Second), m.Revision+1
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateMachine: %v", err)
	}
	if m, err := rowOf(t, a, boxID); err != nil || m.State != machines.StateRunning || m.Revision != 2 {
		t.Fatalf("a reads %+v, %v, want the dead op taken as ended", m, err)
	}
	if _, err := a.Hibernate(t.Context(), boxID); err != nil {
		t.Fatalf("Hibernate over a dead op: %v", err)
	}
	waitState(t, a, boxID, 3, machines.StateHibernated)
}

func TestARefreshEndsALapsedOpAndWakesItsOwner(t *testing.T) {
	fake := newFakeSandboxd(t)
	dsn := clustertest.EmptyDSN(t)
	db, err := cluster.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	b := openSharedBackend(t, fake, db)
	claimRunning(t, b)
	if _, err := db.UpdateMachine(t.Context(), boxID, func(m *cluster.Machine, _ bool) (bool, error) {
		m.Op, m.OpUntil = opHibernate, time.Now().Add(-time.Second)
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateMachine: %v", err)
	}
	fenceListener(t, b, dsn)
	changes := b.Changes(owner)
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	select {
	case <-changes:
	case <-time.After(waitFor):
		t.Fatal("the refresh woke no owner watcher for a lapsed op")
	}
	if rec, err := db.Machine(t.Context(), boxID); err != nil || rec.Op != "" {
		t.Errorf("row after the refresh = op %q, %v; want the lapsed op ended", rec.Op, err)
	}
}

func TestASingleNodeMakesNoMeshCallOnClaimWakeOrExec(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	h, err := b.Hibernate(t.Context(), boxID)
	if err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	waitState(t, b, boxID, h.Revision, machines.StateHibernated)
	w, _, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	waitState(t, b, boxID, w.Revision, machines.StateRunning)
	if err := b.Deliver(t.Context(), boxID, machines.Identity{BoxID: boxID, WakeID: "w1"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	for _, call := range []string{"claim", "hibernate", "wake", "exec"} {
		if n := fake.count(call); n != 1 {
			t.Errorf("%s calls = %d, want 1", call, n)
		}
	}
	if fake.lastClaim().NoRedirect || fake.lastClaim().RequirePromoted {
		t.Errorf("claim = %+v, want no redirect fields", fake.lastClaim())
	}
	clustertest.WaitFor(t, "a peers read only beside each full list", func() bool { return fake.count("peers") == fake.count("list") })
	e, err := b.stored(t.Context(), boxID)
	if err != nil {
		t.Fatalf("stored: %v", err)
	}
	b.nodeMu.Lock()
	nodes := len(b.nodes)
	b.nodeMu.Unlock()
	if e.ownerAddr != fake.addr() || nodes != 0 {
		t.Errorf("owner %q, node clients %d, want the entry node only", e.ownerAddr, nodes)
	}
	if fake.count("info") != 1 || fake.count("owner") != 0 {
		t.Errorf("info reads %d, owner asks %d, want one info read at start and no owner ask", fake.count("info"), fake.count("owner"))
	}
}

func TestAClaimTheEntryRedirectsRunsOnThePeerAndStaysAfterARestart(t *testing.T) {
	entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, peer.peers = []*fakeSandboxd{peer}, []*fakeSandboxd{entry}
	entry.redirect = []*fakeSandboxd{peer}
	db := clustertest.Open(t)
	b := newSharedBackend(t, entry, db)
	m := claimRunning(t, b)
	if entry.count("claim") != 0 || peer.count("claim") != 1 || !peer.lastClaim().NoRedirect {
		t.Fatalf("claims on the entry %d, on the peer %d (no_redirect %t), want the peer's only", entry.count("claim"), peer.count("claim"), peer.lastClaim().NoRedirect)
	}
	if _, err := b.Hibernate(t.Context(), boxID); err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	waitState(t, b, boxID, m.Revision+1, machines.StateHibernated)
	if peer.count("hibernate") != 1 || entry.count("hibernate") != 0 {
		t.Errorf("hibernates on the peer %d, the entry %d, want the peer's", peer.count("hibernate"), entry.count("hibernate"))
	}
	restarted := newSharedBackend(t, entry, db)
	if got, err := restarted.Get(t.Context(), boxID); err != nil || got.State != machines.StateHibernated {
		t.Fatalf("a restarted backend reads %+v, %v, want the hibernated machine on the peer", got, err)
	}
	if _, action, err := restarted.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge}); err != nil || action != machines.ActionWoken {
		t.Fatalf("Ensure on the restarted backend = %v, %v", action, err)
	}
	waitState(t, restarted, boxID, 0, machines.StateRunning)
	if peer.count("wake") != 1 || entry.count("wake") != 0 {
		t.Errorf("wakes on the peer %d, the entry %d, want the peer's", peer.count("wake"), entry.count("wake"))
	}
}

func TestAClaimWhosePeerFailsRunsOnTheEntryWithoutARedirect(t *testing.T) {
	entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	entry.redirect = []*fakeSandboxd{peer}
	peer.setDown(true)
	b := newTestBackend(t, entry)
	claimRunning(t, b)
	if entry.count("claim") != 1 || !entry.lastClaim().NoRedirect {
		t.Errorf("claims on the entry = %d, want one with no_redirect", entry.count("claim"))
	}
}

func TestAMachineOfAPeerThatLeftTheMeshGoesOnlyAfterItStaysAway(t *testing.T) {
	for _, listed := range []bool{false, true} {
		t.Run(map[bool]string{false: "any node", true: "a configured node"}[listed], func(t *testing.T) {
			entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
			entry.peers, entry.redirect = []*fakeSandboxd{peer}, []*fakeSandboxd{peer}
			opts := Options{URL: entry.srv.URL, Token: rootToken, Image: template, Flavors: []machines.Flavor{xlarge}, DiskGiB: 40, AgentPort: 7443, Store: clustertest.Open(t)}
			if listed {
				opts.Nodes = []string{peer.addr()}
			}
			b, err := New(t.Context(), opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			go b.Run(t.Context())
			claimRunning(t, b)
			peer.setDown(true)
			if err := b.refresh(t.Context()); err != nil {
				t.Fatalf("refresh with the peer down: %v", err)
			}
			if _, err := rowOf(t, b, boxID); err != nil {
				t.Fatalf("the machine of a peer that does not answer went at once: %v", err)
			}
			entry.mu.Lock()
			entry.peers = nil
			entry.mu.Unlock()
			if err := b.refresh(t.Context()); err != nil {
				t.Fatalf("refresh after the peer left: %v", err)
			}
			if _, err := rowOf(t, b, boxID); err != nil {
				t.Fatalf("the machine went before its node stayed away: %v", err)
			}
			b.mu.Lock()
			for addr, since := range b.away {
				b.away[addr] = since.Add(-nodeLostAfter)
			}
			b.mu.Unlock()
			if err := b.refresh(t.Context()); err != nil {
				t.Fatalf("refresh after the peer stayed away: %v", err)
			}
			if _, err := rowOf(t, b, boxID); !errors.Is(err, machines.ErrNotFound) {
				t.Errorf("the machine of a node away for good = %v, want it gone", err)
			}
		})
	}
}

func TestAFailedEntryListDropsNothingAndTheRefreshReadsThePeers(t *testing.T) {
	entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, peer.next = []*fakeSandboxd{peer}, 100
	b := newTestBackend(t, entry)
	claimRunning(t, b)
	entry.mu.Lock()
	entry.redirect = []*fakeSandboxd{peer}
	entry.mu.Unlock()
	m, _, err := b.Ensure(t.Context(), machines.Spec{ID: "cb-peer", Owner: owner, Flavor: xlarge})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	waitState(t, b, "cb-peer", m.Revision, machines.StateRunning)
	entry.setDown(true)
	peer.mutate(func(sb *fakeSandbox) { sb.row.Hibernated = true })
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh with the entry down: %v", err)
	}
	if _, err := rowOf(t, b, boxID); err != nil {
		t.Errorf("the machine of an entry whose list failed went: %v", err)
	}
	if got, err := rowOf(t, b, "cb-peer"); err != nil || got.State != machines.StateHibernated {
		t.Errorf("the machine of the peer = %+v, %v, want the hibernation the peer listed", got, err)
	}
}

func TestARollingSwitchOfTheEntryNodeKeepsEveryMachineOnItsNode(t *testing.T) {
	a, b := newFakeSandboxd(t), newFakeSandboxd(t)
	a.peers, b.peers = []*fakeSandboxd{b}, []*fakeSandboxd{a}
	db := clustertest.Open(t)
	before := newSharedBackend(t, a, db)
	claimRunning(t, before)
	a.setDown(true)
	after := newSharedBackend(t, b, db)
	if _, err := rowOf(t, after, boxID); err != nil {
		t.Fatalf("an api entering through the other node dropped the machine of a node that did not answer: %v", err)
	}
	a.setDown(false)
	var version int64
	for i, be := range []*Backend{before, after, before, after} {
		if err := be.refresh(t.Context()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		rec, err := db.Machine(t.Context(), boxID)
		if err != nil || rec.OwnerAddr != a.addr() || (i > 0 && rec.Version != version) {
			t.Fatalf("after refresh %d the row names %q at version %d (was %d), %v; want %q unchanged", i, rec.OwnerAddr, rec.Version, version, err, a.addr())
		}
		version = rec.Version
	}
	h, err := after.Hibernate(t.Context(), boxID)
	if err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	waitState(t, before, boxID, h.Revision, machines.StateHibernated)
	if a.count("hibernate") != 1 || b.count("hibernate") != 0 {
		t.Errorf("hibernates on the owner %d, on the new entry %d, want the owner's", a.count("hibernate"), b.count("hibernate"))
	}
}

func TestAMachineWhoseNodeIsGoneFollowsTheNodeThatAdoptsIt(t *testing.T) {
	entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, entry.redirect = []*fakeSandboxd{peer}, []*fakeSandboxd{peer}
	b := newTestBackend(t, entry)
	claimRunning(t, b)
	h, err := b.Hibernate(t.Context(), boxID)
	if err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	waitState(t, b, boxID, h.Revision, machines.StateHibernated)
	peer.shift(entry, entry.adoptable)
	peer.srv.Close()
	entry.mu.Lock()
	entry.peers = nil
	entry.mu.Unlock()
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	w, action, err := b.Ensure(t.Context(), machines.Spec{ID: boxID, Owner: owner, Flavor: xlarge})
	if err != nil || action != machines.ActionWoken {
		t.Fatalf("Ensure after the node went = %+v, %v, %v, want the machine woken where it was adopted", w, action, err)
	}
	waitState(t, b, boxID, w.Revision, machines.StateRunning)
	if entry.count("owner") == 0 || entry.count("wake") != 1 || entry.count("claim") != 0 {
		t.Errorf("on the adopter: owner asks %d, wakes %d, claims %d; want it adopted and woken, no new claim", entry.count("owner"), entry.count("wake"), entry.count("claim"))
	}
	if e, err := b.stored(t.Context(), boxID); err != nil || e.ownerAddr != entry.addr() {
		t.Errorf("owner %+v, %v, want the adopter %q", e, err, entry.addr())
	}
}

func TestARefreshFollowsAMachineOfALostNodeInsteadOfDroppingIt(t *testing.T) {
	entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, entry.redirect = []*fakeSandboxd{peer}, []*fakeSandboxd{peer}
	b := newTestBackend(t, entry)
	claimRunning(t, b)
	peer.shift(entry, entry.adoptable)
	peer.srv.Close()
	entry.mu.Lock()
	entry.peers = nil
	entry.mu.Unlock()
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh after the node left: %v", err)
	}
	b.mu.Lock()
	for addr, since := range b.away {
		b.away[addr] = since.Add(-nodeLostAfter)
	}
	b.mu.Unlock()
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh after the node stayed away: %v", err)
	}
	if got, err := rowOf(t, b, boxID); err != nil || got.State != machines.StateHibernated {
		t.Fatalf("the machine of the lost node = %+v, %v, want it archived on the adopter", got, err)
	}
	if entry.count("owner") == 0 {
		t.Error("the refresh asked no node for the owner")
	}
}

func TestAMachineOnAFullNodeIsSqueezedUntilTheNodeHasRoom(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	for _, step := range []struct {
		name string
		info map[string]any
		want bool
	}{
		{"room", nil, false},
		{"at capacity", map[string]any{"at_capacity": true, "at_capacity_reason": "memory below reserve"}, true},
		{"running cap reached", map[string]any{"running": 3, "max_running": 3}, true},
		{"disk below reserve", map[string]any{"at_capacity": true, "at_capacity_reason": "disk below reserve"}, false},
		{"room again", map[string]any{"running": 2, "max_running": 3}, false},
	} {
		fake.mu.Lock()
		fake.info = step.info
		fake.mu.Unlock()
		if err := b.readRoom(t.Context()); err != nil {
			t.Fatalf("%s: readRoom: %v", step.name, err)
		}
		ms, err := b.List(t.Context(), "")
		if err != nil || len(ms) != 1 || ms[0].Squeezed != step.want {
			t.Errorf("%s: List = %+v, %v, want one machine squeezed %t", step.name, ms, err, step.want)
		}
		if m, err := b.Get(t.Context(), boxID); err != nil || m.Squeezed != step.want {
			t.Errorf("%s: Get squeezed = %t, %v, want %t", step.name, m.Squeezed, err, step.want)
		}
	}
}

func TestAnAPIWithoutTheTokenLeavesAHandedOverMachineToTheRefresh(t *testing.T) {
	entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, entry.redirect = []*fakeSandboxd{peer}, []*fakeSandboxd{peer}
	db := clustertest.Open(t)
	other := openSharedBackend(t, entry, db)
	a := newSharedBackend(t, entry, db)
	claimRunning(t, a)
	if m, err := rowOf(t, other, boxID); err != nil || m.State != machines.StateRunning {
		t.Fatalf("the other api reads %+v, %v, want the running claim", m, err)
	}
	peer.shift(entry, entry.adoptable)
	if _, err := other.Get(t.Context(), boxID); err != nil {
		t.Fatalf("Get on an api without the token = %v, want the recorded machine", err)
	}
	if _, err := rowOf(t, other, boxID); err != nil {
		t.Fatalf("the api without the token dropped the machine: %v", err)
	}
	if err := a.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got, err := rowOf(t, a, boxID); err != nil || got.State != machines.StateHibernated {
		t.Errorf("the handed-over machine = %+v, %v, want it adopted and hibernated", got, err)
	}
}

func TestARefreshFollowsAMachineItsAnsweringNodeHandedOver(t *testing.T) {
	entry, peer := newFakeSandboxd(t), newFakeSandboxd(t)
	entry.peers, entry.redirect = []*fakeSandboxd{peer}, []*fakeSandboxd{peer}
	b := newTestBackend(t, entry)
	claimRunning(t, b)
	peer.shift(entry, entry.adoptable)
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh after the hand-over: %v", err)
	}
	if got, err := rowOf(t, b, boxID); err != nil || got.State != machines.StateHibernated {
		t.Fatalf("the machine its node handed over = %+v, %v, want it archived on the adopter", got, err)
	}
	if e, err := b.stored(t.Context(), boxID); err != nil || e.ownerAddr != entry.addr() {
		t.Errorf("owner %+v, %v, want the adopter %q", e, err, entry.addr())
	}
}

func TestASnapshotKeepsTheTokensKeptAfterItsRead(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := openSharedBackend(t, fake, clustertest.Open(t))
	read := time.Now()
	claimRunning(t, b)
	b.forgetTokens(map[string]*entry{}, read)
	if b.token(boxID, fake.only()) == "" {
		t.Fatal("a snapshot read before the claim forgot its token")
	}
	b.forgetTokens(map[string]*entry{}, time.Now())
	if b.token(boxID, fake.only()) != "" {
		t.Error("a snapshot read after the claim that lacks it kept its token")
	}
}

func TestARefreshKeepsAClaimThatLandsAfterItsSnapshot(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := openSharedBackend(t, fake, clustertest.Open(t))
	known, err := b.snapshot(t.Context())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	addrs := b.mesh(t.Context())
	lists, err := b.listMesh(t.Context(), addrs)
	if err != nil {
		t.Fatalf("listMesh: %v", err)
	}
	claimRunning(t, b)
	if err := b.dropUnlisted(t.Context(), lists, known); err != nil {
		t.Fatalf("dropUnlisted: %v", err)
	}
	if _, err := rowOf(t, b, boxID); err != nil {
		t.Errorf("a claim that landed after the list = %v, want it kept", err)
	}
}

func TestAChangeWhileTheListenerIsDownWakesEveryOwnerOnTheReconnect(t *testing.T) {
	fake := newFakeSandboxd(t)
	db, dsn := clustertest.OpenDSN(t)
	b := newSharedBackend(t, fake, db)
	fenceListener(t, b, dsn)
	mine, others := b.Changes(owner), b.Changes("bob")
	execSQL(t, dsn, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'LISTEN%'")
	if _, err := db.UpdateMachine(t.Context(), "cb-missed", func(m *cluster.Machine, _ bool) (bool, error) {
		m.Owner, m.CreatedAt = owner, time.Now()
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateMachine: %v", err)
	}
	for name, ch := range map[string]<-chan struct{}{owner: mine, "bob": others} {
		select {
		case <-ch:
		case <-time.After(reconnectWithin):
			t.Errorf("%s was not woken within %s of the listener's reconnect", name, reconnectWithin)
		}
	}
}

func TestARefreshDropsAMachineNoNodeAnswersFor(t *testing.T) {
	fake := newFakeSandboxd(t)
	b := newTestBackend(t, fake)
	claimRunning(t, b)
	fake.remove(fake.only())
	if err := b.refresh(t.Context()); err != nil {
		t.Fatalf("refresh after the release: %v", err)
	}
	if _, err := rowOf(t, b, boxID); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("a machine no node answers for = %v, want it gone", err)
	}
	if fake.count("owner") == 0 {
		t.Error("the refresh dropped the machine without asking the node for the owner")
	}
}

func TestAFingerprintIsKeyedAndStaysTheSame(t *testing.T) {
	v := &envVar{Value: "p&ss<word>", Guest: new(false), Inject: &inject{Hosts: []string{"api.example.com"}, Header: "Authorization", Placeholder: "Bearer <key>"}}
	raw, _ := json.Marshal(v)
	got := fingerprint(rootToken, v)
	if got != fingerprint(rootToken, v) || got == fingerprint("other-token", v) || got == utils.HashHex(string(raw)) {
		t.Errorf("fingerprint = %s, want one stable under the key, another under another key, and not the plain sha256", got)
	}
}

type fakeSandbox struct {
	row     row
	token   string
	env     map[string]envVar
	missing map[string]bool
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
	getStatus   int
	refusals    int
	hold        chan struct{}
	agent       string
	tenants     map[string]tenantRequest
	adoptable   map[string]*fakeSandbox
	peers       []*fakeSandboxd
	redirect    []*fakeSandboxd
	down        bool
	info        map[string]any
}

func newFakeSandboxd(t testing.TB) *fakeSandboxd {
	t.Helper()
	f := &fakeSandboxd{sandboxes: map[string]*fakeSandbox{}, calls: map[string]int{}, tenants: map[string]tenantRequest{}, adoptable: map[string]*fakeSandbox{}}
	mux := f.handler()
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down := f.down
		f.mu.Unlock()
		if down {
			writeError(w, http.StatusServiceUnavailable, "node down")
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSandboxd) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", f.root(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls["info"]++
		resp := map[string]any{"advertise_addr": f.addr()}
		maps.Copy(resp, f.info)
		writeJSON(w, resp)
	}))
	mux.HandleFunc("GET /v1/sandboxes/{id}/owner", func(w http.ResponseWriter, r *http.Request) {
		id, token := r.PathValue("id"), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls["owner"]++
		if sb := f.adoptable[id]; sb != nil && sb.token == token {
			sb.row.Archived = true
			f.sandboxes[id] = sb
			delete(f.adoptable, id)
		}
		if sb := f.sandboxes[id]; sb == nil || sb.token != token {
			writeError(w, http.StatusNotFound, "not owned here")
			return
		}
		writeJSON(w, map[string]string{"owner_addr": f.addr()})
	})
	mux.HandleFunc("GET /v1/peers", f.root(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls["peers"]++
		writeJSON(w, map[string]any{"peers": addrs(f.peers)})
	}))
	mux.HandleFunc("POST /v1/claim", func(w http.ResponseWriter, r *http.Request) {
		if f.hold != nil {
			<-f.hold
		}
		var req claimRequest
		_ = json.UnmarshalRead(r.Body, &req)
		f.mu.Lock()
		if len(f.redirect) > 0 && !req.NoRedirect {
			to := addrs(f.redirect)
			f.mu.Unlock()
			writeJSON(w, map[string]any{"redirect": to})
			return
		}
		tenant, ok := f.tokenOwner(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if !ok {
			f.mu.Unlock()
			writeError(w, http.StatusUnauthorized, "bad token")
			return
		}
		if f.refusals > 0 {
			f.refusals--
			f.mu.Unlock()
			writeError(w, http.StatusTooManyRequests, "node claim quota reached: 12 live claims, cap 12")
			return
		}
		f.next++
		sb := &fakeSandbox{row: row{ID: "sb_" + strconv.Itoa(f.next), Key: req.poolKey, Tenant: tenant, Metadata: req.Metadata, ClaimedAt: time.Now().UTC()}, token: "tok", env: map[string]envVar{}}
		f.sandboxes[sb.row.ID] = sb
		f.claims = append(f.claims, req)
		f.calls["claim"]++
		f.mu.Unlock()
		writeJSON(w, map[string]string{"id": sb.row.ID, "token": sb.token, "owner_addr": f.addr()})
	})
	mux.HandleFunc("PUT /v1/tenants/{name}", f.root(func(w http.ResponseWriter, r *http.Request) {
		var req tenantRequest
		_ = json.UnmarshalRead(r.Body, &req)
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
	mux.HandleFunc("GET /v1/sandboxes", f.root(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls["list"]++
		rows := []row{}
		for _, sb := range f.sandboxes {
			rows = append(rows, sb.row)
		}
		writeJSON(w, map[string]any{"sandboxes": rows})
	}))
	mux.HandleFunc("GET /v1/sandboxes/{id}", f.sandbox(func(w http.ResponseWriter, _ *http.Request, sb *fakeSandbox) {
		if f.getStatus != 0 {
			writeError(w, f.getStatus, "read failed")
			return
		}
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
			_ = json.UnmarshalRead(r.Body, &req)
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
		out := map[string]envVar{}
		for k, v := range sb.env {
			if v.Guest != nil && !*v.Guest {
				v.Value = ""
			}
			v.Missing = sb.missing[k]
			out[k] = v
		}
		writeJSON(w, map[string]any{"env": out})
	}))
	mux.HandleFunc("PATCH /v1/sandboxes/{id}/env", f.sandbox(func(w http.ResponseWriter, r *http.Request, sb *fakeSandbox) {
		var req struct {
			Env map[string]*envVar `json:"env"`
		}
		_ = json.UnmarshalRead(r.Body, &req)
		f.calls["patch"]++
		if f.patchStatus != 0 {
			writeError(w, f.patchStatus, "refused")
			return
		}
		for k, v := range req.Env {
			if v != nil && v.Value == "" && v.Guest != nil && !*v.Guest {
				writeError(w, http.StatusBadRequest, "env "+k+": a host-only entry needs a value")
				return
			}
		}
		for k, v := range req.Env {
			if v == nil {
				delete(sb.env, k)
			} else {
				sb.env[k] = *v
			}
			delete(sb.missing, k)
		}
		sb.row.EnvMissing = len(sb.missing) > 0
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
	_ = json.UnmarshalRead(r.Body, &req)
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

func (f *fakeSandboxd) addr() string {
	return strings.TrimPrefix(f.srv.URL, "http://")
}

func (f *fakeSandboxd) shift(peer *fakeSandboxd, dst map[string]*fakeSandbox) {
	f.mu.Lock()
	claims := f.sandboxes
	f.sandboxes = map[string]*fakeSandbox{}
	f.mu.Unlock()
	peer.mu.Lock()
	defer peer.mu.Unlock()
	maps.Copy(dst, claims)
}

func (f *fakeSandboxd) add(sb *fakeSandbox) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb.env == nil {
		sb.env = map[string]envVar{}
	}
	f.sandboxes[sb.row.ID] = sb
}

func (f *fakeSandboxd) lose(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.sandboxes[id]
	v := sb.env[name]
	v.Value = ""
	sb.env[name] = v
	if sb.missing == nil {
		sb.missing = map[string]bool{}
	}
	sb.missing[name] = true
	sb.row.EnvMissing = true
}

func (f *fakeSandboxd) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, id)
}

func (f *fakeSandboxd) mutate(fn func(*fakeSandbox)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, sb := range f.sandboxes {
		fn(sb)
	}
}

func (f *fakeSandboxd) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
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

func seed(t *testing.T, b *Backend, fake *fakeSandboxd, sb *fakeSandbox) {
	t.Helper()
	fake.add(sb)
	r := sb.row
	env := map[string]string{}
	for name := range sb.env {
		env[name] = ""
	}
	id := r.Metadata["cb_machine"]
	if _, err := b.mutate(t.Context(), id, func(*entry) (*entry, bool) {
		return &entry{id: id, owner: r.Metadata["cb_owner"], image: r.Key.Template, row: &r, ownerAddr: b.entryAddr, meta: map[string]string{}, env: env}, true
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func newTestBackend(t *testing.T, fake *fakeSandboxd) *Backend {
	t.Helper()
	return newSharedBackend(t, fake, clustertest.Open(t))
}

func newSharedBackend(t testing.TB, fake *fakeSandboxd, db *cluster.Store) *Backend {
	t.Helper()
	b := openSharedBackend(t, fake, db)
	go b.Run(t.Context())
	return b
}

func openSharedBackend(t testing.TB, fake *fakeSandboxd, db *cluster.Store) *Backend {
	t.Helper()
	b, err := New(t.Context(), Options{URL: fake.srv.URL, Token: rootToken, Image: template, Flavors: []machines.Flavor{xlarge}, DiskGiB: 40, AgentPort: 7443, Store: db})
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
	ctx, cancel := context.WithTimeout(t.Context(), waitFor)
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

func rowOf(t *testing.T, b *Backend, id string) (machines.Machine, error) {
	t.Helper()
	e, err := b.stored(t.Context(), id)
	if err != nil {
		return machines.Machine{}, err
	}
	return b.machine(e), nil
}

func fenceListener(t *testing.T, b *Backend, dsn string) {
	t.Helper()
	id := "fence-" + strings.ToLower(rand.Text())
	fenced := b.changes(id)
	execSQL(t, dsn, fmt.Sprintf(`SELECT pg_notify('%s', '{"ID":"%s"}')`, cluster.ChannelMachines, id))
	select {
	case <-fenced:
	case <-time.After(waitFor):
		t.Fatal("the listener took no notify within 5 s")
	}
}

func execSQL(t *testing.T, dsn, sql string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	if _, err := conn.Exec(t.Context(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func addrs(nodes []*fakeSandboxd) []string {
	out := []string{}
	for _, n := range nodes {
		out = append(out, n.addr())
	}
	return out
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func isNotRunning(err error, state machines.State) bool {
	nr, ok := errors.AsType[*machines.NotRunningError](err)
	return ok && nr.State == state
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, map[string]string{"error": message})
}
