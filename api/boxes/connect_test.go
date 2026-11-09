package boxes

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
)

func TestProvisioningIssuesADoorKeyPerIncarnationAndPushesTheCredentials(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm1"})
	conn := &fakeConnect{}
	svc := newService(fake, testDefaults, newFakeRegistry())
	svc.EnableConnect(conn)

	svc.provisionStale(t.Context())
	cur, _ := fake.Get(t.Context(), aliceBox)
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm2", Meta: cur.Meta})
	svc.provisionStale(t.Context())

	execs := fake.execCalls()
	if len(execs) != 2 || execs[0].connectKey != "door:"+aliceBox+":vm1" || execs[1].connectKey != "door:"+aliceBox+":vm2" {
		t.Fatalf("deliveries = %+v, want one door key per incarnation", execs)
	}
	creds := slices.DeleteFunc(fake.credCalls(), func(c credCall) bool {
		_, vault := c.flags[vaultFlag]
		return vault
	})
	if len(creds) != 2 || creds[1].secrets["GH_GIT"] != "basic-"+alice || creds[1].flags["COCOONBOX_GH_GIT"] != "1" {
		t.Errorf("credential pushes = %+v, want secrets and flags on each provisioned incarnation", creds)
	}
}

func TestADoorKeyFailureDoesNotFailProvisioning(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm1"})
	svc := newService(fake, testDefaults, newFakeRegistry())
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
	svc := newService(fake, testDefaults, newFakeRegistry())
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
	fake.put(machines.Machine{ID: BoxID(bob), Owner: bob, State: machines.StateRunning})
	conn := &fakeConnect{}
	svc := newService(fake, testDefaults, newFakeRegistry())
	svc.EnableConnect(conn)

	svc.refreshCredentials(t.Context())
	if reads := conn.owners(); !slices.Equal(reads, []string{alice, bob}) {
		t.Errorf("credential reads = %v, want one per owner", reads)
	}
	var pushed []string
	for _, c := range fake.credCalls() {
		pushed = append(pushed, c.machine)
	}
	slices.Sort(pushed)
	if want := []string{aliceBox, aliceBox + "-2", BoxID(bob)}; !slices.Equal(pushed, want) {
		t.Errorf("pushed to %v, want %v", pushed, want)
	}
}

func TestRefreshRotatesOnlyAStaleDoorKeyOfTheCurrentIncarnation(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm1"})
	fake.put(machines.Machine{ID: aliceBox + "-2", Owner: alice, State: machines.StateRunning, Incarnation: "vm2"})
	fake.put(machines.Machine{ID: BoxID(bob), Owner: bob, State: machines.StateRunning, Incarnation: "vm1"})
	conn := &fakeConnect{stale: map[string]string{aliceBox: "vm1", aliceBox + "-2": "vm1"}}
	svc := newService(fake, testDefaults, newFakeRegistry())
	svc.EnableConnect(conn)

	svc.refreshCredentials(t.Context())
	doors := map[string]string{}
	for _, c := range fake.credCalls() {
		if c.secrets["GH_GIT"] == "" {
			t.Errorf("push to %s = %+v, want the owner's credentials", c.machine, c.secrets)
		}
		doors[c.machine] = c.secrets[connectKeyEnv]
	}
	want := map[string]string{aliceBox: "Bearer door:" + aliceBox + ":vm1:rotated", aliceBox + "-2": "", BoxID(bob): ""}
	if !maps.Equal(doors, want) {
		t.Errorf("door keys pushed = %v, want %v", doors, want)
	}
	if rotated := conn.rotations(); !slices.Equal(rotated, []string{aliceBox}) {
		t.Errorf("rotated = %v, want %s only", rotated, aliceBox)
	}
}

func TestApplyReachesOnlyTheRunningMachinesOfTheUser(t *testing.T) {
	for _, tt := range []struct {
		name   string
		enable func(*Service)
		apply  func(*Service, context.Context, string) error
		digest string
	}{
		{"credentials", func(s *Service) { s.EnableConnect(&fakeConnect{}) }, (*Service).ApplyCredentials, ""},
		{"vault", func(s *Service) { s.EnableVault(&fakeVault{digests: map[string]string{alice: "d1"}}) }, (*Service).ApplyVault, "d1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeMachines()
			fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning})
			fake.put(machines.Machine{ID: aliceBox + "-2", Owner: alice, State: machines.StateHibernated})
			fake.put(machines.Machine{ID: BoxID(bob), Owner: bob, State: machines.StateRunning})
			svc := newService(fake, testDefaults, newFakeRegistry())
			tt.enable(svc)
			if err := tt.apply(svc, t.Context(), alice); err != nil {
				t.Fatalf("apply: %v", err)
			}
			flags := map[string]bool{}
			for _, c := range fake.credCalls() {
				flags[c.machine] = c.flags != nil
			}
			if len(flags) != 1 || !flags[aliceBox] {
				t.Errorf("pushes = %v, want alice's running machine only", flags)
			}
			running, _ := fake.Get(t.Context(), aliceBox)
			hibernated, _ := fake.Get(t.Context(), aliceBox+"-2")
			if running.Meta[metaVault] != tt.digest || hibernated.Meta[metaVault] != "" {
				t.Errorf("digests = %q and %q, want %q only on the running machine", running.Meta[metaVault], hibernated.Meta[metaVault], tt.digest)
			}
		})
	}
}

func TestAHibernatedMachineKeepsItsDoorKeyAndADeletedOneLosesIt(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning})
	fake.put(machines.Machine{ID: aliceBox + "-rnew", Owner: alice, State: machines.StateHibernated})
	fake.put(machines.Machine{ID: BoxID(bob), Owner: bob, State: machines.StateRunning})
	conn := &fakeConnect{}
	svc := newService(fake, testDefaults, newFakeRegistry())
	svc.EnableConnect(conn)

	if _, err := svc.Hibernate(auth.WithUser(t.Context(), alice), &controlv1.BoxRef{BoxId: aliceBox}); err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	if err := svc.PruneDoorKeys(t.Context()); err != nil {
		t.Fatalf("PruneDoorKeys: %v", err)
	}
	if dropped, live := conn.doorKeys(); len(dropped) != 0 || !slices.Equal(live, []string{aliceBox, aliceBox + "-rnew", BoxID(bob)}) {
		t.Fatalf("dropped %v, prune kept %v; want nothing dropped and every machine kept", dropped, live)
	}
	if err := svc.Release(t.Context(), alice); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if dropped, _ := conn.doorKeys(); !slices.Equal(dropped, []string{aliceBox, aliceBox + "-rnew"}) {
		t.Errorf("dropped %v, want both of alice's machines", dropped)
	}
}

func TestAParkDropsTheDoorKeyOfTheMachineItRemovesAndTheRestoreGetsANewOne(t *testing.T) {
	e := newParkEnv(t)
	conn := &fakeConnect{}
	e.svc.EnableConnect(conn)

	op := e.park(t, e.clock.Add(parkAfter))
	if op == nil || lastEvent(op).GetPhase() != controlv1.Phase_PHASE_DONE {
		t.Fatalf("park = %v", op)
	}
	if dropped, _ := conn.doorKeys(); !slices.Equal(dropped, []string{aliceBox}) {
		t.Fatalf("dropped %v after the park, want the parked machine", dropped)
	}
	if _, err := e.svc.Ensure(e.ctx, &controlv1.EnsureRequest{BoxId: aliceBox}); err != nil {
		t.Fatalf("Ensure after the park: %v", err)
	}
	spec := e.fake.waitSpec(t, 2)
	e.fake.push(machines.Machine{ID: spec.ID, State: machines.StateRunning, Incarnation: "vm2"})
	e.idle(t)
	if dropped, _ := conn.doorKeys(); !slices.Equal(dropped, []string{aliceBox}) {
		t.Errorf("dropped %v after the restore, want only the parked machine", dropped)
	}
	execs := e.fake.execCalls()
	if len(execs) == 0 || execs[len(execs)-1].connectKey != "door:"+spec.ID+":vm2" {
		t.Errorf("deliveries = %+v, want the restored machine's own door key last", execs)
	}
}

func TestAnEmptyListingPrunesNoDoorKey(t *testing.T) {
	conn := &fakeConnect{}
	svc := newService(newFakeMachines(), testDefaults, newFakeRegistry())
	svc.EnableConnect(conn)

	if err := svc.PruneDoorKeys(t.Context()); err != nil {
		t.Fatalf("PruneDoorKeys: %v", err)
	}
	if conn.prunes != 0 {
		t.Errorf("prunes = %d on an empty listing, want 0", conn.prunes)
	}
}

func waitCreds(t *testing.T, fake *fakeMachines, n int) []credCall {
	t.Helper()
	clustertest.WaitFor(t, fmt.Sprintf("%d credential pushes", n), func() bool { return len(fake.credCalls()) >= n })
	return fake.credCalls()
}

type fakeConnect struct {
	keyErr error
	gate   chan struct{}
	stale  map[string]string

	mu      sync.Mutex
	reads   []string
	dropped []string
	live    []string
	prunes  int
	rotated []string
}

func (c *fakeConnect) DoorKey(_ context.Context, machine, _, incarnation string) (string, error) {
	if c.keyErr != nil {
		return "", c.keyErr
	}
	return "door:" + machine + ":" + incarnation, nil
}

func (c *fakeConnect) DropDoorKey(_ context.Context, machine string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropped = append(c.dropped, machine)
	return nil
}

func (c *fakeConnect) PruneDoorKeys(_ context.Context, live []string, _ time.Time) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live = live
	c.prunes++
	return 0, nil
}

func (c *fakeConnect) StaleDoorKeys(context.Context, time.Duration) (map[string]string, error) {
	return c.stale, nil
}

func (c *fakeConnect) RotateDoorKey(_ context.Context, machine, _, incarnation string, push func(key string) error) error {
	c.mu.Lock()
	c.rotated = append(c.rotated, machine)
	c.mu.Unlock()
	return push("door:" + machine + ":" + incarnation + ":rotated")
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

func (c *fakeConnect) rotations() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.rotated)
}

func (c *fakeConnect) doorKeys() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(slices.Values(c.dropped)), slices.Sorted(slices.Values(c.live))
}
