package boxes

import (
	"cmp"
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/utils"
)

const (
	idPrefix      = "cb-"
	idHexLen      = 12
	machineSuffix = "-r"
)

var (
	boxStates = map[machines.State]controlv1.BoxState{
		machines.StateStarting:    controlv1.BoxState_BOX_STATE_STARTING,
		machines.StateRunning:     controlv1.BoxState_BOX_STATE_RUNNING,
		machines.StateHibernating: controlv1.BoxState_BOX_STATE_HIBERNATING,
		machines.StateHibernated:  controlv1.BoxState_BOX_STATE_HIBERNATED,
		machines.StateFailed:      controlv1.BoxState_BOX_STATE_FAILED,
	}

	stateWords = map[machines.State]string{
		machines.StateStarting:    "starting",
		machines.StateRunning:     "running",
		machines.StateHibernating: "hibernating",
		machines.StateHibernated:  "hibernated",
		machines.StateFailed:      "failed",
	}

	failureWords = map[controlv1.Phase]string{
		controlv1.Phase_PHASE_CREATING:     "machine failed to start",
		controlv1.Phase_PHASE_WAKING:       "wake failed",
		controlv1.Phase_PHASE_SNAPSHOTTING: "snapshot failed",
	}
)

// BoxID is the id of the first box users.Create adds for user; its first machine carries the same id.
func BoxID(user string) string {
	return idPrefix + utils.HashHex(user)[:idHexLen]
}

// newBoxID draws the id of a new box; it never carries machineSuffix.
func newBoxID() string {
	return idPrefix + utils.RandomHex(idHexLen/2)
}

func newMachineID(box string) string {
	return box + machineSuffix + strconv.FormatInt(time.Now().Unix(), 36)
}

// boxOfMachine returns the box a machine serves: its id up to the suffix of a replacement machine.
func boxOfMachine(machine string) string {
	box, _, _ := strings.Cut(machine, machineSuffix)
	return box
}

func older(a, b machines.Machine) bool {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID)) < 0
}

func serving(ms []machines.Machine) map[string]machines.Machine {
	current := map[string]machines.Machine{}
	for _, m := range ms {
		box := boxOfMachine(m.ID)
		if cur, ok := current[box]; !ok || older(m, cur) {
			current[box] = m
		}
	}
	return current
}

// holdsMemory reports whether m counts against the running limit.
func holdsMemory(m machines.Machine) bool {
	return m.State == machines.StateStarting || m.State == machines.StateRunning || m.State == machines.StateHibernating
}

func toBox(box string, m machines.Machine) *controlv1.Box {
	if m.ID == "" {
		return &controlv1.Box{Id: box}
	}
	return &controlv1.Box{
		Id:    box,
		State: boxStates[m.State],
		Image: m.Image,
	}
}

func toObservation(m machines.Machine, phase controlv1.Phase) observation {
	obs := observation{state: boxStates[m.State], detail: stateWords[m.State]}
	switch {
	case atCapacity(m):
		obs.reason = machines.ErrCapacity.Error()
	case m.State == machines.StateFailed:
		obs.reason = cmp.Or(failureWords[phase], "machine failed")
	}
	return obs
}

func atCapacity(m machines.Machine) bool {
	return m.State == machines.StateFailed && m.Reason == machines.ErrCapacity.Error()
}

func startFailure(err error) string {
	if errors.Is(err, machines.ErrCapacity) {
		return machines.ErrCapacity.Error()
	}
	return "new machine did not start"
}

func every(ctx context.Context, interval time.Duration, fn func()) {
	tick := time.Tick(interval)
	for {
		fn()
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
