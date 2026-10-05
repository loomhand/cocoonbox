package sandboxd

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	metaMachine = "cb_machine"
	metaOwner   = "cb_owner"

	envMetaPrefix = "CB_META_"
	envWakes      = "CB_WAKES"
	envGWKey      = "GW_KEY"
	envConnectKey = "CB_CONNECT_KEY"
	envUpstream   = "EGRESS_UPSTREAM"

	opClaim     = "claim"
	opWake      = "wake"
	opHibernate = "hibernate"
)

var tiers = []struct {
	size   string
	flavor machines.Flavor
}{
	{"small", machines.Flavor{VCPUs: 1, MemoryMiB: 512}},
	{"medium", machines.Flavor{VCPUs: 2, MemoryMiB: 1024}},
	{"large", machines.Flavor{VCPUs: 4, MemoryMiB: 4096}},
	{"xlarge", machines.Flavor{VCPUs: 4, MemoryMiB: 8192}},
	{"2xlarge", machines.Flavor{VCPUs: 8, MemoryMiB: 16384}},
}

// entry is the backend's record of one machine: its claim row and what the backend knows beyond it.
type entry struct {
	id    string
	owner string
	image string
	// created stands in for the claim time until the claim row exists.
	created time.Time
	row     *row

	op       string
	opErr    string
	revision int64

	meta map[string]string
	// env maps each env name the claim holds to the fingerprint of the entry this process wrote, "" when unknown.
	env    map[string]string
	wakes  int
	routed bool
	// ready means the agent answered on its port since the claim, a wake or the last restart.
	ready         bool
	readyRestarts int
}

func (e *entry) state() (machines.State, string) {
	switch {
	case e.op == opClaim, e.op == opWake:
		return machines.StateStarting, ""
	case e.op == opHibernate:
		return machines.StateHibernating, ""
	case e.opErr != "":
		return machines.StateFailed, e.opErr
	case e.row == nil:
		return machines.StateStarting, ""
	case e.row.Hibernated || e.row.Archived:
		return machines.StateHibernated, ""
	case e.row.Failed != "":
		return machines.StateFailed, e.row.Failed
	case !e.ready || e.readyRestarts != e.row.Restarts:
		return machines.StateStarting, ""
	default:
		return machines.StateRunning, ""
	}
}

func (e *entry) machine(disk uint32) machines.Machine {
	state, reason := e.state()
	m := machines.Machine{
		ID:       e.id,
		Owner:    e.owner,
		State:    state,
		Image:    e.image,
		Reason:   reason,
		Revision: e.revision,
		Meta:     maps.Clone(e.meta),
		Routed:   e.routed,
	}
	if m.Meta == nil {
		m.Meta = map[string]string{}
	}
	if e.row == nil {
		m.CreatedAt, m.Detail = e.created, "claim="+e.op
		return m
	}
	m.Image = e.row.Key.Template
	m.Flavor = flavorOf(e.row.Key.Size, disk)
	m.CreatedAt = e.row.ClaimedAt
	m.Incarnation = fmt.Sprintf("%d.%d.%d", e.row.ClaimedAt.UnixMilli(), e.row.Restarts, e.wakes)
	m.Detail = fmt.Sprintf("sandbox=%s hibernated=%t archived=%t restarts=%d ready=%t op=%s", e.row.ID,
		e.row.Hibernated, e.row.Archived, e.row.Restarts, e.ready, e.op)
	if e.row.Failed != "" {
		m.Detail += " failed: " + e.row.Failed
	}
	return m
}

// loadEnv reads the service meta, the wake count, whether an upstream is set and the names from the claim's env entries.
func (e *entry) loadEnv(env map[string]envVar) {
	e.meta = map[string]string{}
	e.env = map[string]string{}
	for name, v := range env {
		e.env[name] = ""
		if key, ok := strings.CutPrefix(name, envMetaPrefix); ok {
			e.meta[key] = v.Value
		}
	}
	e.wakes, _ = strconv.Atoi(env[envWakes].Value)
	_, e.routed = env[envUpstream]
}

// sizeOf returns the tier of f; disk is not part of a tier.
func sizeOf(f machines.Flavor) (string, bool) {
	for _, t := range tiers {
		if t.flavor.VCPUs == f.VCPUs && t.flavor.MemoryMiB == f.MemoryMiB {
			return t.size, true
		}
	}
	return "", false
}

func flavorOf(size string, disk uint32) machines.Flavor {
	for _, t := range tiers {
		if t.size == size {
			f := t.flavor
			f.DiskGiB = disk
			return f
		}
	}
	return machines.Flavor{DiskGiB: disk}
}
