package sandboxd

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	metaMachine     = "cb_machine"
	metaOwner       = "cb_owner"
	metaClaimPrefix = "cb_meta_"

	envUpstream = "EGRESS_UPSTREAM"

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
	{"medium-4g", machines.Flavor{VCPUs: 2, MemoryMiB: 4096}},
	{"large", machines.Flavor{VCPUs: 4, MemoryMiB: 4096}},
	{"xlarge", machines.Flavor{VCPUs: 4, MemoryMiB: 8192}},
	{"2xlarge", machines.Flavor{VCPUs: 8, MemoryMiB: 16384}},
}

// placement names a claim on the node that holds it.
type placement struct {
	addr    string
	sandbox string
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
	case e.opErr == machines.ReasonHibernateFailed && e.row != nil && (e.row.Hibernated || e.row.Archived):
		return machines.StateHibernated, ""
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

func (e *entry) failedOnLiveClaim() bool {
	return e.opErr != "" && e.row != nil && !e.row.Hibernated && !e.row.Archived && e.row.Failed == ""
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

func (e *entry) machine() machines.Machine {
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
	m.EnvMissing = e.row.EnvMissing
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

// sizeOf returns the tier of f; disk is not part of a tier.
func sizeOf(f machines.Flavor) (string, bool) {
	for _, t := range tiers {
		if t.flavor.VCPUs == f.VCPUs && t.flavor.MemoryMiB == f.MemoryMiB {
			return t.size, true
		}
	}
	return "", false
}
