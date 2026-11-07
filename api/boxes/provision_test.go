package boxes

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/credential"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

func TestEnsureProvisionsBeforeRunning(t *testing.T) {
	fake := newFakeMachines()
	accounts := &fakeAccounts{}
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableAccounts(accounts)
	svc.EnableProvisioning(fakeSigner{})
	ctx := auth.WithUser(t.Context(), alice)

	resp, err := svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if spec := fake.lastSpec(); spec.Tenant != "tenant-"+alice {
		t.Errorf("claimed with tenant %q, want the user's", spec.Tenant)
	}
	fake.push(machines.Machine{ID: aliceBox, State: machines.StateRunning, Incarnation: "pod1/vm1"})
	events := watch(t, svc, ctx, resp.GetOperation().GetId(), "")
	last := events[len(events)-1]
	if last.GetPhase() != controlv1.Phase_PHASE_DONE || last.GetBoxState() != controlv1.BoxState_BOX_STATE_RUNNING {
		t.Fatalf("last event = %v", last)
	}
	if !slices.ContainsFunc(events, func(ev *controlv1.OperationEvent) bool { return ev.GetDetail() == "provisioning" }) {
		t.Errorf("no provisioning event in %v", events)
	}

	execs := fake.execCalls()
	if len(execs) != 1 || execs[0].gwKey != "gw-key-"+alice+"-ak1" {
		t.Fatalf("deliveries = %+v, want one with the incarnation's gw key", execs)
	}
	identity := execs[0].identity
	if identity.BoxID != aliceBox || identity.Name != users.FirstBoxName || identity.KeyID != "k1" || identity.PublicKey != "pub1" || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(identity.WakeID) {
		t.Errorf("identity = %+v", identity)
	}
	if accounts.calls() != "key "+aliceBox+" pod1/vm1" {
		t.Errorf("account calls = %q", accounts.calls())
	}
	ep, wake, err := svc.Resolve(ctx, alice, aliceBox)
	if err != nil || wake != identity.WakeID || ep.Key == "" {
		t.Errorf("Resolve = %q, %q, %v, want wake %s", ep.Key, wake, err, identity.WakeID)
	}
}

func TestWakeReprovisionsTheIdentityWithANewGWKey(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "pod1/vm1", Meta: map[string]string{metaWake: "pod1/vm1|w-old"}})
	accounts := &fakeAccounts{}
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableAccounts(accounts)
	svc.EnableProvisioning(fakeSigner{})
	ctx := auth.WithUser(t.Context(), alice)

	if _, wake, err := svc.Resolve(ctx, alice, aliceBox); err != nil || wake != "w-old" {
		t.Fatalf("Resolve before wake = %q, %v", wake, err)
	}
	if _, err := svc.Rename(ctx, &controlv1.RenameBoxRequest{BoxId: aliceBox, Name: "desk_1"}); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "pod1/vm2", Meta: map[string]string{metaWake: "pod1/vm1|w-old"}})
	if _, _, err := svc.Resolve(ctx, alice, aliceBox); status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "box is starting; call box_ensure to bring it up" {
		t.Errorf("Resolve of an unprovisioned incarnation: %v, want Unavailable box is starting; call box_ensure to bring it up", err)
	}
	if box, err := svc.get(ctx, aliceBox); err != nil || box.GetState() != controlv1.BoxState_BOX_STATE_STARTING {
		t.Errorf("Get = %v, %v, want STARTING", box, err)
	}

	go svc.RunProvisioner(t.Context(), 10*time.Millisecond)
	clustertest.WaitFor(t, "the provisioner provisions the new incarnation", func() bool {
		_, wake, err := svc.Resolve(ctx, alice, aliceBox)
		return err == nil && wake != "w-old"
	})
	if accounts.calls() != "key "+aliceBox+" pod1/vm2" {
		t.Errorf("account calls = %q, want one gw key for the new incarnation", accounts.calls())
	}
	if execs := fake.execCalls(); len(execs) != 1 || execs[0].gwKey == "" || execs[0].identity.WakeID == "w-old" || execs[0].identity.Name != "desk_1" {
		t.Errorf("deliveries = %+v, want one identity named desk_1 with a gw key", execs)
	}
}

func TestProvisioningCoversEveryUser(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "pod1/vm1"})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableAccounts(&fakeAccounts{})
	svc.EnableProvisioning(fakeSigner{})
	if _, _, err := svc.Resolve(auth.WithUser(t.Context(), alice), alice, aliceBox); status.Code(err) != codes.Unavailable {
		t.Errorf("Resolve before provisioning: %v, want Unavailable", err)
	}
	svc.provisionStale(t.Context())
	if execs := fake.execCalls(); len(execs) != 1 || execs[0].identity.BoxID != aliceBox {
		t.Fatalf("deliveries = %+v, want one for %s", execs, aliceBox)
	}
	if _, wake, err := svc.Resolve(auth.WithUser(t.Context(), alice), alice, aliceBox); err != nil || wake == "" {
		t.Errorf("Resolve after provisioning = %q, %v", wake, err)
	}
}

func TestProvisioningFailureFailsTheOperation(t *testing.T) {
	fake := newFakeMachines()
	fake.execErr = errors.New("exec: 502")
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableProvisioning(fakeSigner{})
	svc.provision.backoff = time.Millisecond
	ctx := auth.WithUser(t.Context(), alice)
	resp, err := svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	fake.push(machines.Machine{ID: aliceBox, State: machines.StateRunning, Incarnation: "pod1/vm1"})
	events := watch(t, svc, ctx, resp.GetOperation().GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_FAILED || last.GetDetail() != "machine failed to start" {
		t.Errorf("last event = %v", last)
	}
}

func TestRebaseClaimsUnderTheTenant(t *testing.T) {
	env := newStateEnv(t)
	accounts := &fakeAccounts{}
	env.fake.put(machines.Machine{
		ID: aliceBox, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor, Image: "desktop:v1", Revision: 1,
		Incarnation: "pod0/vm0", Meta: map[string]string{metaWake: "pod0/vm0|w0"},
	})
	env.svc.EnableAccounts(accounts)
	env.svc.EnableProvisioning(fakeSigner{})
	env.agent.chunks = []string{hexID("c1")}
	op, err := env.svc.Rebase(env.ctx, &controlv1.RebaseRequest{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	spec := env.fake.waitSpec(t, 1)
	newID := spec.ID
	if spec.Tenant != "tenant-"+alice {
		t.Errorf("rebase machine claimed with tenant %q, want the user's", spec.Tenant)
	}
	env.fake.push(machines.Machine{ID: newID, State: machines.StateRunning, Incarnation: "pod1/vm1"})
	events := watch(t, env.svc, env.ctx, op.GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("last event = %v", last)
	}
	if got := env.agent.planCredential(); got != "cred:"+aliceBox+":w0" {
		t.Errorf("save on the old machine sent credential %q", got)
	}
	restoreCred := env.agent.restoreCredential()
	if !strings.HasPrefix(restoreCred, "cred:"+aliceBox+":") || restoreCred == "cred:"+aliceBox+":w0" {
		t.Errorf("restore on the new machine sent credential %q", restoreCred)
	}
	if want := "key " + newID + " pod1/vm1"; accounts.calls() != want {
		t.Errorf("account calls = %q, want %q", accounts.calls(), want)
	}
}

func TestARestoredMachineGetsItsOwnIDAndItsOwnGWKey(t *testing.T) {
	env := newStateEnv(t)
	env.agent.chunks = []string{hexID("c1")}
	env.save(t)
	accounts := &fakeAccounts{}
	env.svc.EnableAccounts(accounts)
	env.svc.EnableProvisioning(fakeSigner{})
	env.fake.mu.Lock()
	delete(env.fake.machines, aliceBox)
	env.fake.mu.Unlock()

	resp, err := env.svc.Ensure(env.ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	newID := env.fake.waitSpec(t, 1).ID
	if !strings.HasPrefix(newID, aliceBox+"-r") {
		t.Fatalf("restored machine id = %q, want %s-r<ts>", newID, aliceBox)
	}
	env.fake.push(machines.Machine{ID: newID, State: machines.StateRunning, Incarnation: "pod1/vm1"})
	if last := watch(t, env.svc, env.ctx, resp.GetOperation().GetId(), ""); last[len(last)-1].GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("restore ended with %v", last[len(last)-1])
	}
	if want := "key " + newID + " pod1/vm1"; accounts.calls() != want {
		t.Errorf("account calls = %q, want %q", accounts.calls(), want)
	}
}

func TestReleaseDeletesEveryMachineOfTheUser(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning})
	fake.put(machines.Machine{ID: aliceBox + "-r1", Owner: alice, State: machines.StateRunning})
	fake.put(machines.Machine{ID: BoxID("bob"), Owner: "bob", State: machines.StateRunning})
	svc := New(fake, testDefaults, newFakeRegistry())
	if err := svc.Release(t.Context(), alice); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if ms, _ := fake.List(t.Context(), alice); len(ms) != 0 {
		t.Errorf("alice keeps %d machines", len(ms))
	}
	if ms, _ := fake.List(t.Context(), "bob"); len(ms) != 1 {
		t.Errorf("bob has %d machines, want 1", len(ms))
	}
}

func TestTheProvisionerDeletesAMachineAnInterruptedRebaseLeftOnlyOnceNoOperationRuns(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "pod1/vm1", Meta: map[string]string{metaWake: "pod1/vm1|w1"}})
	stray := aliceBox + "-r1"
	fake.put(machines.Machine{ID: stray, Owner: alice, State: machines.StateRunning, Incarnation: "pod2/vm2", Meta: map[string]string{metaWake: "pod2/vm2|w2"}})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableProvisioning(fakeSigner{})
	op, _ := svc.ops.start(t.Context(), alice, aliceBox, controlv1.Phase_PHASE_SAVING, 1, time.Minute)
	svc.provisionStale(t.Context())
	if got := fake.deletedIDs(); got != "" {
		t.Fatalf("the sweep deleted %q while an operation ran on the box", got)
	}
	op.fail("interrupted")
	svc.provisionStale(t.Context())
	if got := fake.deletedIDs(); got != stray {
		t.Errorf("the sweep deleted %q, want the stray %s", got, stray)
	}
}

func TestTheProvisionerPushesTheLostHostKeysAgainAndKeepsTheIdentity(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "pod1/vm1", Routed: true, EnvMissing: true, Meta: map[string]string{metaWake: "pod1/vm1|w1", metaVault: "d1"}})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableAccounts(&fakeAccounts{})
	svc.EnableProvisioning(fakeSigner{})
	svc.EnableConnect(&fakeConnect{})
	svc.EnableVault(&fakeVault{digests: map[string]string{alice: "d1"}})
	svc.EnableResidential("http://proxy:3128", func(string) bool { return true })
	ctx := auth.WithUser(t.Context(), alice)
	if _, _, err := svc.Resolve(ctx, alice, aliceBox); status.Code(err) != codes.Unavailable {
		t.Fatalf("Resolve of a machine that lost its keys: %v, want Unavailable", err)
	}

	svc.provisionStale(t.Context())
	if execs := fake.execCalls(); len(execs) != 0 {
		t.Errorf("deliveries = %+v, want the identity left alone", execs)
	}
	pushed := map[string]bool{}
	for _, c := range fake.credCalls() {
		for name := range c.secrets {
			pushed[name] = true
		}
	}
	for _, name := range []string{gwKeyEnv, connectKeyEnv, "GH_GIT", "CBV_1"} {
		if !pushed[name] {
			t.Errorf("pushed %v, want %s among them", pushed, name)
		}
	}
	if got := fake.egressCalls(); !slices.Contains(got, aliceBox+" http://proxy:3128") {
		t.Errorf("egress = %v, want the upstream set again", got)
	}
	if _, wake, err := svc.Resolve(ctx, alice, aliceBox); err != nil || wake != "w1" {
		t.Errorf("Resolve after the push = %q, %v, want wake w1", wake, err)
	}
}

func TestAMachineStaysUnreachableWhileAPushFailsAndItsMissingEntries(t *testing.T) {
	fake := newFakeMachines()
	fake.keepMissing = true
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "pod1/vm1", EnvMissing: true, Meta: map[string]string{metaWake: "pod1/vm1|w1"}})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableProvisioning(fakeSigner{})
	svc.EnableVault(&fakeVault{digests: map[string]string{alice: "d1"}, err: errors.New("vault down")})
	svc.provision.backoff = time.Millisecond

	m, _ := fake.Get(t.Context(), aliceBox)
	if err := svc.provisionMachine(t.Context(), m, false); !errors.Is(err, errEnvMissing) {
		t.Errorf("provisionMachine = %v, want errEnvMissing", err)
	}
	if _, _, err := svc.Resolve(auth.WithUser(t.Context(), alice), alice, aliceBox); status.Code(err) != codes.Unavailable {
		t.Errorf("Resolve: %v, want Unavailable", err)
	}
	if m, _ := fake.Get(t.Context(), aliceBox); !svc.ready(m) || fake.drops != 0 {
		t.Errorf("ready = %t after %d drops, want saves and idling to reach the agent and nothing dropped", svc.ready(m), fake.drops)
	}
}

func TestEntriesNothingPushesAgainAreRemovedSoTheMachineServes(t *testing.T) {
	fake := newFakeMachines()
	fake.keepMissing = true
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "pod1/vm1", EnvMissing: true, Meta: map[string]string{metaWake: "pod1/vm1|w1"}})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableProvisioning(fakeSigner{})

	m, _ := fake.Get(t.Context(), aliceBox)
	if err := svc.provisionMachine(t.Context(), m, false); err != nil || fake.drops != 1 {
		t.Fatalf("provisionMachine = %v after %d drops, want one drop", err, fake.drops)
	}
	if _, wake, err := svc.Resolve(auth.WithUser(t.Context(), alice), alice, aliceBox); err != nil || wake != "w1" {
		t.Errorf("Resolve = %q, %v", wake, err)
	}
}

func TestAFailedPushAgainRevokesEveryKeyAndAGWOutageDoesNotBlockIt(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "i1", EnvMissing: true, Meta: map[string]string{metaWake: "i1|w1"}})
	accounts := &fakeAccounts{keys: []users.MachineKey{{ID: "ak-old", Machine: aliceBox, Incarnation: "i1"}}}
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableAccounts(accounts)
	svc.EnableProvisioning(fakeSigner{})
	svc.provision.backoff = time.Millisecond
	fake.credErr = errors.New("node 502")

	m, _ := fake.Get(t.Context(), aliceBox)
	if err := svc.provisionMachine(t.Context(), m, false); err == nil {
		t.Fatal("provisionMachine succeeded with a failing push")
	}
	if keys, _ := accounts.MachineKeys(t.Context(), ""); len(keys) != 0 {
		t.Errorf("keys = %+v, want every key revoked so the next pass keys the machine again", keys)
	}
	fake.credErr = nil
	accounts.issueErr = errors.New("gw admin down")
	if err := svc.provisionMachine(t.Context(), m, false); err != nil {
		t.Fatalf("provisionMachine during a gw outage = %v, want the machine provisioned without a key", err)
	}
	m, _ = fake.Get(t.Context(), aliceBox)
	if err := svc.provisionMachine(t.Context(), m, true); err == nil {
		t.Error("a rekey during a gw outage succeeded")
	}
}

func TestProvisioningLeavesAMachineThatStoppedRunning(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Incarnation: "i1", Meta: map[string]string{metaWake: "i1|w1"}})
	accounts := &fakeAccounts{}
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableAccounts(accounts)
	svc.EnableProvisioning(fakeSigner{})

	m, _ := fake.Get(t.Context(), aliceBox)
	if _, ok := errors.AsType[*machines.NotRunningError](svc.provisionMachine(t.Context(), m, true)); !ok || accounts.calls() != "" {
		t.Errorf("provisionMachine of a hibernated machine: NotRunning %t, calls %q; want NotRunning and no key", ok, accounts.calls())
	}
}

func TestTheProvisionerRevokesTheKeysOfEndedIncarnationsAndKeysARunningOneAgain(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "i2", Meta: map[string]string{metaWake: "i2|w2"}})
	fake.put(machines.Machine{ID: aliceBox + "-2", Owner: alice, State: machines.StateHibernated, Incarnation: "j1", Meta: map[string]string{metaWake: "j1|w1"}})
	accounts := &fakeAccounts{keys: []users.MachineKey{{ID: "ak-old", Machine: aliceBox, Incarnation: "i1"}, {ID: "ak-asleep", Machine: aliceBox + "-2", Incarnation: "j1"}}}
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableAccounts(accounts)
	svc.EnableProvisioning(fakeSigner{})

	svc.provisionStale(t.Context())
	if want := "revoke ak-old;revoke ak-asleep;key " + aliceBox + " i2"; accounts.calls() != want {
		t.Errorf("account calls = %q, want %q", accounts.calls(), want)
	}
	if execs := fake.execCalls(); len(execs) != 0 {
		t.Errorf("deliveries = %+v, want the identity left alone", execs)
	}
	if creds := fake.credCalls(); len(creds) != 1 || creds[0].secrets[gwKeyEnv] != "Bearer gw-key-"+alice+"-ak1" {
		t.Errorf("pushes = %+v, want the new gw key", creds)
	}
	svc.provisionStale(t.Context())
	if keys, _ := accounts.MachineKeys(t.Context(), ""); len(keys) != 1 || len(fake.credCalls()) != 1 {
		t.Errorf("keys = %+v after %d pushes, want one key and no second push", keys, len(fake.credCalls()))
	}
}

func TestAFailedProvisioningRevokesTheKeyItIssuedAndADeleteRevokesTheRest(t *testing.T) {
	fake := newFakeMachines()
	fake.execErr = errors.New("exec: 502")
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "i1"})
	accounts := &fakeAccounts{}
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableAccounts(accounts)
	svc.EnableProvisioning(fakeSigner{})
	svc.provision.backoff = time.Millisecond

	m, _ := fake.Get(t.Context(), aliceBox)
	if err := svc.provisionMachine(t.Context(), m, false); err == nil {
		t.Fatal("provisionMachine succeeded with a failing delivery")
	}
	if keys, _ := accounts.MachineKeys(t.Context(), ""); len(keys) != 0 {
		t.Errorf("keys after the failures = %+v, want each issued key revoked", keys)
	}
	fake.execErr = nil
	if err := svc.provisionMachine(t.Context(), m, false); err != nil {
		t.Fatalf("provisionMachine: %v", err)
	}
	if err := svc.deleteMachine(t.Context(), aliceBox); err != nil {
		t.Fatalf("deleteMachine: %v", err)
	}
	if keys, _ := accounts.MachineKeys(t.Context(), ""); len(keys) != 0 {
		t.Errorf("keys after the delete = %+v, want none", keys)
	}
}

func TestEveryPushOfASecretIsRecordedWithItsNode(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "i1", Node: "n1"})
	accounts := &fakeAccounts{}
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableAccounts(accounts)
	svc.EnableProvisioning(fakeSigner{})
	svc.EnableConnect(&fakeConnect{})
	svc.EnableVault(&fakeVault{digests: map[string]string{alice: "d1", "bob": ""}})

	svc.provisionStale(t.Context())
	want := []string{aliceBox + " " + alice + " n1 connect", aliceBox + " " + alice + " n1 door", aliceBox + " " + alice + " n1 model", aliceBox + " " + alice + " n1 vault"}
	if got := accounts.pushed(); !slices.Equal(got, want) {
		t.Errorf("pushes = %v, want %v", got, want)
	}
	fake.put(machines.Machine{ID: BoxID("bob"), Owner: "bob", State: machines.StateRunning, Incarnation: "j1", Node: "n2", Meta: map[string]string{metaWake: "j1|w1"}})
	before := len(accounts.pushed())
	if err := svc.ApplyVault(t.Context(), "bob"); err != nil {
		t.Fatalf("ApplyVault: %v", err)
	}
	if got := accounts.pushed(); len(got) != before {
		t.Errorf("pushes = %v, want none for a vault without a secret", got)
	}
}

func outgoingCredential(ctx context.Context) string {
	md, _ := metadata.FromOutgoingContext(ctx)
	if v := md.Get(credential.Header); len(v) == 1 {
		return v[0]
	}
	return ""
}

type fakeSigner struct{}

func (fakeSigner) KeyID() string                      { return "k1" }
func (fakeSigner) PublicKey() string                  { return "pub1" }
func (fakeSigner) Credential(box, wake string) string { return "cred:" + box + ":" + wake }

type fakeAccounts struct {
	mu       sync.Mutex
	log      []string
	keys     []users.MachineKey
	next     int
	pushes   []string
	issueErr error
}

func (a *fakeAccounts) Tenant(user string) (string, error) { return "tenant-" + user, nil }

func (a *fakeAccounts) IssueMachineKey(_ context.Context, owner, machine, incarnation string) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.issueErr != nil {
		return "", "", a.issueErr
	}
	a.next++
	id := "ak" + strconv.Itoa(a.next)
	a.keys = slices.Insert(a.keys, 0, users.MachineKey{ID: id, Machine: machine, Incarnation: incarnation})
	a.log = append(a.log, "key "+machine+" "+incarnation)
	return "gw-key-" + owner + "-" + id, id, nil
}

func (a *fakeAccounts) MachineKeys(_ context.Context, machine string) ([]users.MachineKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(a.keys), func(k users.MachineKey) bool { return machine != "" && k.Machine != machine }), nil
}

func (a *fakeAccounts) RevokeMachineKey(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keys = slices.DeleteFunc(a.keys, func(k users.MachineKey) bool { return k.ID == id })
	a.log = append(a.log, "revoke "+id)
	return nil
}

func (a *fakeAccounts) RecordPushes(_ context.Context, machine, owner, node string, kinds []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, kind := range kinds {
		a.pushes = append(a.pushes, machine+" "+owner+" "+node+" "+kind)
	}
	return nil
}

func (a *fakeAccounts) pushed() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Sorted(slices.Values(a.pushes))
}

func (a *fakeAccounts) calls() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.log, ";")
}
