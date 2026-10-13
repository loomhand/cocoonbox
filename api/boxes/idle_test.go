package boxes

import (
	"context"
	"testing"
	"time"

	"github.com/CMGS/cocoonbox/api/machines"
)

const idleAfter = 30 * time.Minute

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

func newIdleService(t *testing.T, agent bool) (*Service, *fakeMachines, *time.Time) {
	t.Helper()
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor, Revision: 1})
	svc := New(fake, testDefaults, newFakeRegistry())
	svc.EnableIdle(idleAfter, func(context.Context) (map[string]bool, error) { return map[string]bool{aliceBox: agent}, nil })
	clock := time.Unix(1_000_000, 0)
	svc.idle.now = func() time.Time { return clock }
	return svc, fake, &clock
}
