package boxes

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

func TestProvisioningIssuesADoorKeyPerIncarnationAndPushesTheCredentials(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm1"})
	conn := &fakeConnect{}
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableProvisioning(fakeSigner{})
	svc.EnableConnect(conn)

	svc.provisionStale(t.Context())
	cur, _ := fake.Get(t.Context(), aliceBox)
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm2", Meta: cur.Meta})
	svc.provisionStale(t.Context())

	execs := fake.execCalls()
	if len(execs) != 2 || execs[0].connectKey != "door:"+aliceBox+":vm1" || execs[1].connectKey != "door:"+aliceBox+":vm2" {
		t.Fatalf("deliveries = %+v, want one door key per incarnation", execs)
	}
	creds := fake.credCalls()
	if len(creds) != 2 || creds[1].secrets["GH_GIT"] != "basic-"+alice || creds[1].flags["COCOONBOX_GH_GIT"] != "1" {
		t.Errorf("credential pushes = %+v, want secrets and flags on each provisioned incarnation", creds)
	}
}

func TestADoorKeyFailureDoesNotFailProvisioning(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm1"})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableProvisioning(fakeSigner{})
	svc.EnableConnect(&fakeConnect{keyErr: errors.New("database down")})

	svc.provisionStale(t.Context())
	if execs := fake.execCalls(); len(execs) != 1 || execs[0].connectKey != "" {
		t.Fatalf("deliveries = %+v, want one without a door key", execs)
	}
	if _, wake, err := svc.Resolve(auth.WithUser(t.Context(), alice), alice, aliceBox); err != nil || wake == "" {
		t.Errorf("Resolve after provisioning = %q, %v", wake, err)
	}
}

func TestAWakePushesTheSecretsWithoutWaitingForThem(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Incarnation: "vm1"})
	conn := &fakeConnect{gate: make(chan struct{})}
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableConnect(conn)

	resp, err := svc.Ensure(auth.WithUser(t.Context(), alice), &controlv1.EnsureRequest{})
	if err != nil || resp.GetOperation().GetId() == "" {
		t.Fatalf("Ensure = %v, %v, want a wake operation", resp, err)
	}
	if got := fake.credCalls(); len(got) != 0 {
		t.Fatalf("pushes before the credentials were read = %+v", got)
	}
	close(conn.gate)
	creds := waitCreds(t, fake, 1)
	if creds[0].secrets["GH_GIT"] != "basic-"+alice || creds[0].flags != nil {
		t.Errorf("wake push = %+v, want the secrets without flags", creds[0])
	}
}

func TestRefreshReadsEachOwnerOnceForItsRunningMachines(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning})
	fake.put(machines.Machine{ID: aliceBox + "-2", Owner: alice, State: machines.StateRunning})
	fake.put(machines.Machine{ID: aliceBox + "-3", Owner: alice, State: machines.StateHibernated})
	fake.put(machines.Machine{ID: BoxID("bob"), Owner: "bob", State: machines.StateRunning})
	conn := &fakeConnect{}
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableConnect(conn)

	svc.refreshCredentials(t.Context())
	if reads := conn.owners(); !slices.Equal(reads, []string{alice, "bob"}) {
		t.Errorf("credential reads = %v, want one per owner", reads)
	}
	var pushed []string
	for _, c := range fake.credCalls() {
		pushed = append(pushed, c.machine)
	}
	slices.Sort(pushed)
	if want := []string{aliceBox, aliceBox + "-2", BoxID("bob")}; !slices.Equal(pushed, want) {
		t.Errorf("pushed to %v, want %v", pushed, want)
	}
}

func TestApplyCredentialsReachesEveryMachineOfTheUser(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning})
	fake.put(machines.Machine{ID: aliceBox + "-2", Owner: alice, State: machines.StateHibernated})
	fake.put(machines.Machine{ID: BoxID("bob"), Owner: "bob", State: machines.StateRunning})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableConnect(&fakeConnect{})

	if err := svc.ApplyCredentials(t.Context(), alice); err != nil {
		t.Fatalf("ApplyCredentials: %v", err)
	}
	flags := map[string]bool{}
	for _, c := range fake.credCalls() {
		flags[c.machine] = c.flags != nil
	}
	if len(flags) != 2 || !flags[aliceBox] || flags[aliceBox+"-2"] {
		t.Errorf("pushes = %v, want both of alice's machines and flags only on the running one", flags)
	}
}

func waitCreds(t *testing.T, fake *fakeMachines, n int) []credCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := fake.credCalls(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d credential pushes", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type fakeConnect struct {
	keyErr error
	gate   chan struct{}

	mu    sync.Mutex
	reads []string
}

func (c *fakeConnect) DoorKey(_ context.Context, machine, _, incarnation string) (string, error) {
	if c.keyErr != nil {
		return "", c.keyErr
	}
	return "door:" + machine + ":" + incarnation, nil
}

func (c *fakeConnect) Credentials(ctx context.Context, owner string) (machines.Credentials, error) {
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return machines.Credentials{}, ctx.Err()
		}
	}
	c.mu.Lock()
	c.reads = append(c.reads, owner)
	c.mu.Unlock()
	return machines.Credentials{
		Secrets: map[string]*machines.Secret{"GH_GIT": {Value: "basic-" + owner}},
		Flags:   map[string]string{"COCOONBOX_GH_GIT": "1"},
	}, nil
}

func (c *fakeConnect) owners() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(slices.Values(c.reads))
}
