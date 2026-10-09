package boxes

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
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

// BoxID returns the id of the first box user had before several boxes per user; its first machine carries the same id.
func BoxID(user string) string {
	sum := sha256.Sum256([]byte(user))
	return idPrefix + hex.EncodeToString(sum[:])[:idHexLen]
}

// newBoxID draws the id of a new box; it never carries machineSuffix.
func newBoxID() string {
	raw := make([]byte, idHexLen/2)
	_, _ = rand.Read(raw)
	return idPrefix + hex.EncodeToString(raw)
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

// holdsMemory reports whether m counts against the running limit.
func holdsMemory(m machines.Machine) bool {
	return m.State == machines.StateStarting || m.State == machines.StateRunning || m.State == machines.StateHibernating
}

func toBox(box string, m machines.Machine) *controlv1.Box {
	if m.ID == "" {
		return &controlv1.Box{Id: box}
	}
	return &controlv1.Box{
		Id:            box,
		State:         boxStates[m.State],
		Image:         m.Image,
		Flavor:        toFlavor(m.Flavor),
		FailureReason: failureReason(m),
		CreatedAtMs:   m.CreatedAt.UnixMilli(),
	}
}

func toObservation(m machines.Machine, phase controlv1.Phase) observation {
	obs := observation{state: boxStates[m.State], detail: progressWords(m)}
	switch {
	case atCapacity(m):
		obs.reason = machines.ErrCapacity.Error()
	case m.State == machines.StateFailed:
		obs.reason = cmp.Or(failureWords[phase], "machine failed")
	}
	return obs
}

func progressWords(m machines.Machine) string {
	if m.Waiting {
		return "waiting for capacity"
	}
	return stateWords[m.State]
}

func failureReason(m machines.Machine) string {
	switch {
	case atCapacity(m):
		return machines.ErrCapacity.Error()
	case m.State == machines.StateFailed:
		return "machine failed"
	}
	return ""
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

func toFlavor(f machines.Flavor) *controlv1.Flavor {
	return &controlv1.Flavor{Vcpus: f.VCPUs, MemoryMib: f.MemoryMiB, DiskGib: f.DiskGiB}
}

func resolveFlavor(req *controlv1.Flavor, def machines.Flavor) machines.Flavor {
	return machines.Flavor{
		VCPUs:     cmp.Or(req.GetVcpus(), def.VCPUs),
		MemoryMiB: cmp.Or(req.GetMemoryMib(), def.MemoryMiB),
		DiskGiB:   cmp.Or(req.GetDiskGib(), def.DiskGiB),
	}
}
