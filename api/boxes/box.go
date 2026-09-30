package boxes

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	idPrefix = "cb-"
	idHexLen = 12
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

// BoxID returns the id of user's box; its first machine carries the same id.
func BoxID(user string) string {
	sum := sha256.Sum256([]byte(user))
	return idPrefix + hex.EncodeToString(sum[:])[:idHexLen]
}

func newMachineID(box string) string {
	return box + "-r" + strconv.FormatInt(time.Now().Unix(), 36)
}

func toBox(box string, m machines.Machine) *controlv1.Box {
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
	if m.State == machines.StateFailed {
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
	if m.State == machines.StateFailed {
		return "machine failed"
	}
	return ""
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
