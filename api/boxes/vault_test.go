package boxes

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

const vaultFlag = "COCOONBOX_VAULT"

func TestTheProvisionerPushesKeysOnceAndAgainOnlyWhenTheDigestMoves(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm1"})
	fake.put(machines.Machine{ID: BoxID(bob), Owner: bob, State: machines.StateRunning, Incarnation: "vm1"})
	keys := &fakeVault{digests: map[string]string{alice: "d1"}}
	svc := newService(fake, testDefaults, newFakeRegistry())
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
	if got := pushesTo(fake, BoxID(bob)); got != 1 {
		t.Errorf("pushes to bob's machine without keys = %d, want only the provisioning push", got)
	}
}

func TestAWakePushesTheKeys(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Incarnation: "vm1"})
	svc := newService(fake, testDefaults, newFakeRegistry())
	svc.EnableVault(&fakeVault{digests: map[string]string{alice: "d1"}})

	if _, err := svc.Ensure(auth.WithUser(t.Context(), alice), &controlv1.EnsureRequest{}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	creds := waitCreds(t, fake, 1)
	if creds[0].secrets["CBV_1"] != "v-"+alice || creds[0].flags != nil {
		t.Errorf("wake push = %+v, want the key without the guest list", creds[0])
	}
}

func TestAKeyPushWaitsForAnotherProcesssProvisioningOfTheMachine(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm1"})
	svc := newService(fake, testDefaults, newFakeRegistry())
	svc.EnableVault(&fakeVault{digests: map[string]string{alice: "d1"}})
	shared := clustertest.Open(t)
	svc.EnableShared(t.Context(), shared)
	held, err := shared.LockProvision(t.Context(), "provision:"+aliceBox)
	if err != nil {
		t.Fatalf("hold the provisioning lock: %v", err)
	}
	release := sync.OnceFunc(held)
	t.Cleanup(release)

	pushed := make(chan error, 1)
	go func() { pushed <- svc.syncVault(t.Context(), aliceBox) }()
	select {
	case err := <-pushed:
		t.Fatalf("syncVault = %v while another process provisions the machine, want it to wait", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-pushed; err != nil || pushesTo(fake, aliceBox) != 1 {
		t.Errorf("syncVault after the lock = %v with %d pushes, want one push", err, pushesTo(fake, aliceBox))
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
	err     error
}

func (v *fakeVault) Digests(context.Context) (map[string]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return maps.Clone(v.digests), nil
}

func (v *fakeVault) Credentials(_ context.Context, owner string) (machines.Credentials, string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return machines.Credentials{}, "", v.err
	}
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
