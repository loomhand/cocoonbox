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

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/users"

	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	idleAfter = 30 * time.Minute
	agentBox  = "cb-a9e47b0c55d1"
)

func TestIdleHibernatesOnlyAnAgentBoxWithNoCallForTheThreshold(t *testing.T) {
	tests := []struct {
		name    string
		agent   bool
		call    func(svc *Service)
		elapsed time.Duration
		want    machines.State
	}{
		{"idle agent box", true, func(*Service) {}, idleAfter, machines.StateHibernating},
		{"under the threshold", true, func(*Service) {}, idleAfter - time.Second, machines.StateRunning},
		{"a call open", true, func(svc *Service) { svc.Reached(aliceBox) }, 2 * idleAfter, machines.StateRunning},
		{"a user's box", false, func(*Service) {}, 2 * idleAfter, machines.StateRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, fake, clock := newIdleService(t, tt.agent)
			svc.hibernateIdle(t.Context())
			tt.call(svc)
			*clock = clock.Add(tt.elapsed)
			svc.hibernateIdle(t.Context())
			if got := fake.state(aliceBox); got != tt.want {
				t.Errorf("state = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIdleCountsFromTheEndOfTheLastCall(t *testing.T) {
	svc, fake, clock := newIdleService(t, true)
	left := svc.Reached(aliceBox)
	*clock = clock.Add(2 * idleAfter)
	left()
	*clock = clock.Add(idleAfter - time.Second)
	svc.hibernateIdle(t.Context())
	if got := fake.state(aliceBox); got != machines.StateRunning {
		t.Fatalf("state = %v right under the threshold after the call ended", got)
	}
	*clock = clock.Add(time.Second)
	svc.hibernateIdle(t.Context())
	if got := fake.state(aliceBox); got != machines.StateHibernating {
		t.Errorf("state = %v at the threshold after the call ended", got)
	}
}

func TestIdleStartsAgainAtAWake(t *testing.T) {
	svc, fake, clock := newIdleService(t, true)
	svc.hibernateIdle(t.Context())
	fake.setState(aliceBox, machines.StateHibernated)
	*clock = clock.Add(2 * idleAfter)
	svc.hibernateIdle(t.Context())
	fake.setState(aliceBox, machines.StateRunning)
	svc.hibernateIdle(t.Context())
	if got := fake.state(aliceBox); got != machines.StateRunning {
		t.Errorf("state = %v on the first pass after a wake", got)
	}
}

func TestAnAgentBoxGetsNoKeysConnectCredentialsOrDoorKey(t *testing.T) {
	fake, svc := newCredentialService(t, func(context.Context) (map[string]bool, error) { return map[string]bool{agentBox: true}, nil })
	svc.provisionStale(t.Context())
	if err := svc.ApplyCredentials(t.Context(), alice); err != nil {
		t.Fatalf("ApplyCredentials: %v", err)
	}
	if err := svc.ApplyVault(t.Context(), alice); err != nil {
		t.Fatalf("ApplyVault: %v", err)
	}
	svc.refreshCredentials(t.Context())
	synctest.Test(t, func(t *testing.T) {
		for _, id := range []string{aliceBox, agentBox} {
			m, _ := fake.Get(t.Context(), id)
			svc.wakeCredentials(t.Context(), m)
		}
		synctest.Wait()
	})

	keys := map[string]string{}
	for _, e := range fake.execCalls() {
		keys[e.machine] = e.connectKey
	}
	if keys[agentBox] != "" || keys[aliceBox] != "door:"+aliceBox+":vm1" {
		t.Errorf("door keys = %v, want one for the owner's box only", keys)
	}
	if n := pushesTo(fake, agentBox); n != 0 {
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

func TestUnreadableAgentBoxesKeepEveryCredentialBack(t *testing.T) {
	fake, svc := newCredentialService(t, func(context.Context) (map[string]bool, error) { return nil, errors.New("database down") })
	svc.provisionStale(t.Context())
	_ = svc.ApplyCredentials(t.Context(), alice)
	_ = svc.ApplyVault(t.Context(), alice)
	for _, e := range fake.execCalls() {
		if e.connectKey != "" {
			t.Errorf("machine %s got a door key", e.machine)
		}
	}
	if calls := fake.credCalls(); len(calls) != 0 {
		t.Errorf("credential pushes = %+v, want none", calls)
	}
}

func TestCreateRecordsTheBoxBeforeItsMachineStarts(t *testing.T) {
	fake := newFakeMachines()
	defaults := testDefaults
	defaults.MaxRunning = 3
	svc := New(fake, defaults, newFakeRegistry())
	ctx := auth.WithUser(t.Context(), alice)
	var recorded string
	created, err := svc.Create(WithCreated(ctx, func(_ context.Context, box string) error {
		if fake.specCount() != 0 {
			t.Error("the machine started before the box was recorded")
		}
		recorded = box
		return nil
	}), &controlv1.CreateBoxRequest{Name: "agent-one"})
	if err != nil || recorded == "" || created.GetBox().GetId() != recorded {
		t.Fatalf("Create = %v, %v, recorded %q", created, err, recorded)
	}

	_, err = svc.Create(WithCreated(ctx, func(context.Context, string) error { return errors.New("database down") }), &controlv1.CreateBoxRequest{Name: "agent-two"})
	if status.Code(err) != codes.Unavailable || fake.specCount() != 1 || slices.ContainsFunc(svc.registry.Boxes(alice), func(b users.Box) bool { return b.Name == "agent-two" }) {
		t.Errorf("Create with a failed record = %v, machines %d, boxes %v", err, fake.specCount(), svc.registry.Boxes(alice))
	}
}

func newCredentialService(t *testing.T, agents AgentBoxes) (*fakeMachines, *Service) {
	t.Helper()
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm1"})
	fake.put(machines.Machine{ID: agentBox, Owner: alice, State: machines.StateRunning, Incarnation: "vm1"})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableProvisioning(fakeSigner{})
	svc.EnableConnect(&fakeConnect{})
	svc.EnableVault(&fakeVault{digests: map[string]string{alice: "d1"}})
	svc.EnableAgentBoxes(agents, 0)
	return fake, svc
}

func newIdleService(t *testing.T, agent bool) (*Service, *fakeMachines, *time.Time) {
	t.Helper()
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor, Revision: 1})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableAgentBoxes(func(context.Context) (map[string]bool, error) { return map[string]bool{aliceBox: agent}, nil }, idleAfter)
	clock := time.Unix(1_000_000, 0)
	svc.idle.now = func() time.Time { return clock }
	return svc, fake, &clock
}
