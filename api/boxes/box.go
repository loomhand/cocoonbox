package boxes

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"

	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	idPrefix = "cb-"
	idHexLen = 12
)

var boxStates = map[machines.State]controlv1.BoxState{
	machines.StateStarting:    controlv1.BoxState_BOX_STATE_STARTING,
	machines.StateRunning:     controlv1.BoxState_BOX_STATE_RUNNING,
	machines.StateHibernating: controlv1.BoxState_BOX_STATE_HIBERNATING,
	machines.StateHibernated:  controlv1.BoxState_BOX_STATE_HIBERNATED,
	machines.StateFailed:      controlv1.BoxState_BOX_STATE_FAILED,
}

// BoxID returns the id of user's box, which is also its machine id.
func BoxID(user string) string {
	sum := sha256.Sum256([]byte(user))
	return idPrefix + hex.EncodeToString(sum[:])[:idHexLen]
}

func toBox(m machines.Machine) *controlv1.Box {
	return &controlv1.Box{
		Id:            m.ID,
		State:         boxStates[m.State],
		Image:         m.Image,
		Flavor:        toFlavor(m.Flavor),
		FailureReason: m.Reason,
		CreatedAtMs:   m.CreatedAt.UnixMilli(),
	}
}

func toObservation(m machines.Machine) observation {
	return observation{state: boxStates[m.State], detail: m.Detail, reason: m.Reason}
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
