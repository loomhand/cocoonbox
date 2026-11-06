package sandboxd

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	metaMachine     = "cb_machine"
	metaOwner       = "cb_owner"
	metaClaimPrefix = "cb_meta_"

	envMetaPrefix = "CB_META_"
	envWakes      = "CB_WAKES"
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

// seenClaim holds a claim's own token, kept in memory only, to ask the mesh which node owns the claim.
type seenClaim struct {
	sandbox string
	token   string
	kept    time.Time
}

// entry is the backend's record of one machine: its claim row and what the backend knows beyond it.
type entry struct {
	id    string
	owner string
	image string
	// created stands in for the claim time until the claim row exists.
	created time.Time
	row     *row
	// ownerAddr names the node that holds the claim as the mesh names it.
	ownerAddr string

	op    string
	opErr string
	// opUntil is when op's process gives up on it, so a later reader takes it as ended.
	opUntil  time.Time
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

func loadEntry(rec cluster.Machine) *entry {
	e := &entry{
		id: rec.ID, owner: rec.Owner, image: rec.Image, created: rec.CreatedAt, ownerAddr: rec.OwnerAddr,
		op: rec.Op, opErr: rec.OpErr, opUntil: rec.OpUntil, revision: rec.Revision,
		meta: maps.Clone(rec.Meta), env: maps.Clone(rec.Env), wakes: rec.Wakes, routed: rec.Routed, ready: rec.Ready, readyRestarts: rec.ReadyRestarts,
	}
	if e.meta == nil {
		e.meta = map[string]string{}
	}
	if rec.Claim != nil {
		var r row
		if json.Unmarshal(rec.Claim, &r) == nil {
			e.row = &r
		}
	}
	return e
}

func (e *entry) state() (machines.State, string) {
	op := e.op
	if !e.busy(time.Now()) {
		op = ""
	}
	switch {
	case op == opClaim, op == opWake:
		return machines.StateStarting, ""
	case op == opHibernate:
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

// sandbox is the id of e's claim, "" for none or a nil e.
func (e *entry) sandbox() string {
	if e == nil || e.row == nil {
		return ""
	}
	return e.row.ID
}

func (e *entry) busy(now time.Time) bool {
	return e.op != "" && now.Before(e.opUntil)
}

func (e *entry) begin(op string, now time.Time, lease time.Duration) {
	e.op, e.opErr, e.opUntil = op, "", now.Add(lease)
	e.revision++
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
	if e.row != nil {
		for k, v := range e.row.Metadata {
			if key, ok := strings.CutPrefix(k, metaClaimPrefix); ok && m.Meta[key] == "" {
				m.Meta[key] = v
			}
		}
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

func (e *entry) record() cluster.Machine {
	m := cluster.Machine{
		ID: e.id, Owner: e.owner, Image: e.image, CreatedAt: e.created, OwnerAddr: e.ownerAddr, Op: e.op, OpErr: e.opErr, OpUntil: e.opUntil,
		Revision: e.revision, Meta: maps.Clone(e.meta), Wakes: e.wakes, Routed: e.routed, Ready: e.ready, ReadyRestarts: e.readyRestarts,
		Env: maps.Clone(e.env),
	}
	if e.row != nil {
		m.Claim, _ = json.Marshal(e.row, json.Deterministic(true))
	}
	return m
}

// loadEnv reads the service meta, the wake count, whether an upstream is set and the names from the claim's env entries.
func (e *entry) loadEnv(env map[string]envVar) {
	e.meta, e.env = map[string]string{}, make(map[string]string, len(env))
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
