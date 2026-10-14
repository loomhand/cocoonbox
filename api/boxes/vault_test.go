package boxes

import (
	"context"
	"maps"
	"sync"
	"testing"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

const vaultFlag = "COCOONBOX_VAULT"

func TestTheProvisionerPushesKeysOnceAndAgainOnlyWhenTheDigestMoves(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm1"})
	fake.put(machines.Machine{ID: BoxID("bob"), Owner: "bob", State: machines.StateRunning, Incarnation: "vm1"})
	keys := &fakeVault{digests: map[string]string{alice: "d1"}}
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableProvisioning(fakeSigner{})
	svc.EnableVault(keys)

	svc.provisionStale(t.Context())
	svc.provisionStale(t.Context())
	if got := pushesTo(fake, aliceBox); got != 1 {
		t.Errorf("pushes to alice's machine after two passes = %d, want 1", got)
	}
	if m, _ := fake.Get(t.Context(), aliceBox); m.Meta[metaVault] != "d1" {
		t.Errorf("recorded digest = %q, want d1", m.Meta[metaVault])
	}
	if c := lastPushTo(fake, aliceBox); c.secrets["CBV_1"] != "v-"+alice || c.flags[vaultFlag] != "K=ph" {
		t.Errorf("push = %+v, want the key and the guest list", c)
	}

	keys.set(alice, "d2")
	svc.provisionStale(t.Context())
	svc.provisionStale(t.Context())
	if got := pushesTo(fake, aliceBox); got != 2 {
		t.Errorf("pushes after the digest moved = %d, want 2", got)
	}
	if m, _ := fake.Get(t.Context(), aliceBox); m.Meta[metaVault] != "d2" {
		t.Errorf("recorded digest = %q, want d2", m.Meta[metaVault])
	}
	if got := pushesTo(fake, BoxID("bob")); got != 1 {
		t.Errorf("pushes to bob's machine without keys = %d, want only the provisioning push", got)
	}
}

func TestApplyVaultReachesHibernatedMachinesWithoutTheGuestList(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning})
	fake.put(machines.Machine{ID: aliceBox + "-2", Owner: alice, State: machines.StateHibernated})
	fake.put(machines.Machine{ID: BoxID("bob"), Owner: "bob", State: machines.StateRunning})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableProvisioning(fakeSigner{})
	svc.EnableVault(&fakeVault{digests: map[string]string{alice: "d1"}})

	if err := svc.ApplyVault(t.Context(), alice); err != nil {
		t.Fatalf("ApplyVault: %v", err)
	}
	flags := map[string]bool{}
	for _, c := range fake.credCalls() {
		flags[c.machine] = c.flags != nil
	}
	if len(flags) != 2 || !flags[aliceBox] || flags[aliceBox+"-2"] {
		t.Errorf("pushes = %v, want both of alice's machines and the guest list only on the running one", flags)
	}
	running, _ := fake.Get(t.Context(), aliceBox)
	hibernated, _ := fake.Get(t.Context(), aliceBox+"-2")
	if running.Meta[metaVault] != "d1" || hibernated.Meta[metaVault] != "" {
		t.Errorf("digests = %q and %q, want one only on the running machine", running.Meta[metaVault], hibernated.Meta[metaVault])
	}
}

func TestAWakePushesTheKeys(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Incarnation: "vm1"})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableProvisioning(fakeSigner{})
	svc.EnableVault(&fakeVault{digests: map[string]string{alice: "d1"}})

	if _, err := svc.Ensure(auth.WithUser(t.Context(), alice), &controlv1.EnsureRequest{}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	creds := waitCreds(t, fake, 1)
	if creds[0].secrets["CBV_1"] != "v-"+alice || creds[0].flags != nil {
		t.Errorf("wake push = %+v, want the key without the guest list", creds[0])
	}
}

func pushesTo(fake *fakeMachines, machine string) int {
	n := 0
	for _, c := range fake.credCalls() {
		if c.machine == machine {
			n++
		}
	}
	return n
}

func lastPushTo(fake *fakeMachines, machine string) credCall {
	var last credCall
	for _, c := range fake.credCalls() {
		if c.machine == machine {
			last = c
		}
	}
	return last
}

type fakeVault struct {
	mu      sync.Mutex
	digests map[string]string
}

func (v *fakeVault) Digests(context.Context) (map[string]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return maps.Clone(v.digests), nil
}

func (v *fakeVault) Credentials(_ context.Context, owner string) (machines.Credentials, string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	c := machines.Credentials{Secrets: map[string]*machines.Secret{}, Flags: map[string]string{vaultFlag: ""}, Prune: "CBV_"}
	if d := v.digests[owner]; d != "" {
		c.Secrets["CBV_1"] = &machines.Secret{Value: "v-" + owner}
		c.Flags[vaultFlag] = "K=ph"
	}
	return c, v.digests[owner], nil
}

func (v *fakeVault) set(owner, digest string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.digests[owner] = digest
}
