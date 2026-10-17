package boxes

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

const idleAfter = 30 * time.Minute

func TestIdleHibernatesOnlyAnAgentBoxWithNoCallForTheThreshold(t *testing.T) {
	tests := []struct {
		name    string
		agent   bool
		open    bool
		elapsed time.Duration
		want    machines.State
	}{
		{"idle agent box", true, false, idleAfter, machines.StateHibernating},
		{"under the threshold", true, false, idleAfter - time.Second, machines.StateRunning},
		{"a call open", true, true, 2 * idleAfter, machines.StateRunning},
		{"a user's box", false, false, 2 * idleAfter, machines.StateRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, fake, clock, box := newIdleService(t, tt.agent)
			svc.hibernateIdle(t.Context())
			if tt.open {
				svc.Reached(box)
			}
			*clock = clock.Add(tt.elapsed)
			svc.hibernateIdle(t.Context())
			if got := fake.state(box); got != tt.want {
				t.Errorf("state = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIdleCountsFromTheEndOfTheLastCall(t *testing.T) {
	svc, fake, clock, box := newIdleService(t, true)
	left := svc.Reached(box)
	*clock = clock.Add(2 * idleAfter)
	left()
	*clock = clock.Add(idleAfter - time.Second)
	svc.hibernateIdle(t.Context())
	if got := fake.state(box); got != machines.StateRunning {
		t.Fatalf("state = %v right under the threshold after the call ended", got)
	}
	*clock = clock.Add(time.Second)
	svc.hibernateIdle(t.Context())
	if got := fake.state(box); got != machines.StateHibernating {
		t.Errorf("state = %v at the threshold after the call ended", got)
	}
}

func TestIdleStartsAgainAtAWake(t *testing.T) {
	svc, fake, clock, box := newIdleService(t, true)
	svc.hibernateIdle(t.Context())
	fake.setState(box, machines.StateHibernated)
	*clock = clock.Add(2 * idleAfter)
	svc.hibernateIdle(t.Context())
	fake.setState(box, machines.StateRunning)
	svc.hibernateIdle(t.Context())
	if got := fake.state(box); got != machines.StateRunning {
		t.Errorf("state = %v on the first pass after a wake", got)
	}
}

func TestAnAgentBoxGetsNoKeysConnectCredentialsOrDoorKey(t *testing.T) {
	svc, fake := newCredentialService(t)
	box := addAgentBox(t, svc, fake)
	svc.provisionStale(t.Context())
	if err := svc.ApplyCredentials(t.Context(), alice); err != nil {
		t.Fatalf("ApplyCredentials: %v", err)
	}
	if err := svc.ApplyVault(t.Context(), alice); err != nil {
		t.Fatalf("ApplyVault: %v", err)
	}
	svc.refreshCredentials(t.Context())
	synctest.Test(t, func(t *testing.T) {
		for _, id := range []string{aliceBox, box} {
			m, _ := fake.Get(t.Context(), id)
			svc.wakeCredentials(t.Context(), m)
		}
		synctest.Wait()
	})

	keys := map[string]string{}
	for _, e := range fake.execCalls() {
		keys[e.machine] = e.connectKey
	}
	if _, ok := keys[box]; !ok || keys[box] != "" || keys[aliceBox] != "door:"+aliceBox+":vm1" {
		t.Errorf("door keys = %v, want one for the owner's box only", keys)
	}
	if n := pushesTo(fake, box); n != 0 {
		t.Errorf("credential pushes to the agent box = %d, want 0", n)
	}
	var vault, connect bool
	for _, c := range fake.credCalls() {
		if c.machine == aliceBox {
			vault = vault || c.secrets["CBV_1"] == "v-"+alice
			connect = connect || c.secrets["GH_GIT"] == "basic-"+alice
		}
	}
	if !vault || !connect {
		t.Errorf("the owner's box got keys %t and Connect credentials %t, want both", vault, connect)
	}
}

func TestAnAgentCreateRecordsItsBoxFirstAndSkipsTheOwnersLimits(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning})
	registry := newFakeRegistry()
	registry.max = 1
	svc := New(fake, testDefaults, registry)
	ctx := auth.WithUser(t.Context(), alice)
	if _, err := svc.Create(ctx, &controlv1.CreateBoxRequest{Name: "mine"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("the owner's Create past the running limit = %v, want FailedPrecondition", err)
	}

	var recorded string
	created, err := svc.Create(WithAgent(ctx, func(_ context.Context, box string) error {
		if fake.specCount() != 0 {
			t.Error("the machine started before the box was recorded")
		}
		recorded = box
		return nil
	}), &controlv1.CreateBoxRequest{Name: "agent-one"})
	if err != nil || recorded == "" || created.GetBox().GetId() != recorded || !svc.agentBox(alice, recorded) {
		t.Fatalf("an agent's Create past the owner's max boxes and running limit = %v, %v, recorded %q", created, err, recorded)
	}

	_, err = svc.Create(WithAgent(ctx, func(context.Context, string) error { return errors.New("database down") }), &controlv1.CreateBoxRequest{Name: "agent-two"})
	if status.Code(err) != codes.Unavailable || fake.specCount() != 1 || slices.ContainsFunc(svc.registry.Boxes(alice), func(b users.Box) bool { return b.Name == "agent-two" }) {
		t.Errorf("Create with a failed record = %v, machines %d, boxes %v", err, fake.specCount(), svc.registry.Boxes(alice))
	}
	if err := svc.checkRunning(alice, recorded, map[string]machines.Machine{aliceBox: {State: machines.StateRunning}}); err != nil {
		t.Errorf("waking the agent box with the owner's box running = %v", err)
	}
	if err := svc.checkRunning(alice, aliceBox, map[string]machines.Machine{recorded: {State: machines.StateRunning}}); err != nil {
		t.Errorf("waking the owner's box with the agent box running = %v", err)
	}
}

func newIdleService(t *testing.T, agent bool) (*Service, *fakeMachines, *time.Time, string) {
	t.Helper()
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor, Revision: 1})
	svc := New(fake, testDefaults, newFakeRegistry())
	box := aliceBox
	if agent {
		box = addAgentBox(t, svc, fake)
	}
	svc.EnableIdle(idleAfter, 0)
	clock := time.Unix(1_000_000, 0)
	svc.idle.now = func() time.Time { return clock }
	return svc, fake, &clock, box
}

func newCredentialService(t *testing.T) (*Service, *fakeMachines) {
	t.Helper()
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm1"})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableProvisioning(fakeSigner{})
	svc.EnableConnect(&fakeConnect{})
	svc.EnableVault(&fakeVault{digests: map[string]string{alice: "d1"}})
	return svc, fake
}

// addAgentBox gives alice a running box an agent created.
func addAgentBox(t *testing.T, svc *Service, fake *fakeMachines) string {
	t.Helper()
	b, err := svc.registry.AddBox(t.Context(), users.Box{ID: newBoxID(), Owner: alice, Name: "agent", Agent: true})
	if err != nil {
		t.Fatalf("add agent box: %v", err)
	}
	fake.put(machines.Machine{ID: b.ID, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor, Incarnation: "vm1", Revision: 1})
	return b.ID
}
