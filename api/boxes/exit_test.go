package boxes

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	bob         = "bob"
	residential = "http://10.203.1.2:8823"
)

func TestProvisioningRoutesOnlyResidentialOwners(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "pod1/vm1"})
	fake.put(machines.Machine{ID: BoxID(bob), Owner: bob, State: machines.StateRunning, Incarnation: "pod2/vm1"})
	svc := New(fake, testDefaults)
	svc.EnableProvisioning(fakeSigner{}, nil)
	svc.EnableResidential(residential, func(_ context.Context, user string) (bool, error) { return user == alice, nil })

	svc.provisionStale(t.Context())
	if got := fake.egressCalls(); !slices.Equal(got, []string{aliceBox + " " + residential}) {
		t.Errorf("egress calls = %v, want only alice's box routed", got)
	}
	if execs := fake.execCalls(); len(execs) != 2 {
		t.Errorf("deliveries = %+v, want both boxes provisioned", execs)
	}
}

func TestProvisionerFollowsTheSwitch(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Incarnation: "pod1/vm1", Meta: map[string]string{metaWake: "pod1/vm1|w1"}})
	var on atomic.Bool
	on.Store(true)
	svc := New(fake, testDefaults)
	svc.EnableProvisioning(fakeSigner{}, nil)
	svc.EnableResidential(residential, func(context.Context, string) (bool, error) { return on.Load(), nil })

	svc.provisionStale(t.Context())
	svc.provisionStale(t.Context())
	on.Store(false)
	svc.provisionStale(t.Context())
	svc.provisionStale(t.Context())
	want := []string{aliceBox + " " + residential, aliceBox + " direct"}
	if got := fake.egressCalls(); !slices.Equal(got, want) {
		t.Errorf("egress calls = %v, want %v", got, want)
	}
	if execs := fake.execCalls(); len(execs) != 0 {
		t.Errorf("deliveries = %+v, want none for a hibernated box", execs)
	}
}

func TestApplyExitRoutesEveryMachineOfTheUserOnly(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning})
	fake.put(machines.Machine{ID: aliceBox + "-r1", Owner: alice, State: machines.StateStarting})
	fake.put(machines.Machine{ID: BoxID(bob), Owner: bob, State: machines.StateRunning})
	svc := New(fake, testDefaults)
	svc.EnableResidential(residential, func(context.Context, string) (bool, error) { return true, nil })

	if err := svc.ApplyExit(t.Context(), alice); err != nil {
		t.Fatalf("ApplyExit: %v", err)
	}
	got := fake.egressCalls()
	slices.Sort(got)
	if want := []string{aliceBox + " " + residential, aliceBox + "-r1 " + residential}; !slices.Equal(got, want) {
		t.Errorf("egress calls = %v, want %v", got, want)
	}
}

func TestASwitchReadFailureHoldsProvisioning(t *testing.T) {
	fake := newFakeMachines()
	fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Incarnation: "pod1/vm1"})
	svc := New(fake, testDefaults)
	svc.EnableProvisioning(fakeSigner{}, nil)
	svc.provision.backoff = 0
	svc.EnableResidential(residential, func(context.Context, string) (bool, error) { return false, errors.New("users table unavailable") })

	m, _ := fake.Get(t.Context(), aliceBox)
	if err := svc.provisionMachine(t.Context(), m); err == nil {
		t.Error("provisioning went ahead without the switch")
	}
	if execs := fake.execCalls(); len(execs) != 0 {
		t.Errorf("deliveries = %+v, want none", execs)
	}
}
