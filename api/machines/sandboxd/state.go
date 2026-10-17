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
	metaMachine = "cb_machine"
	metaOwner   = "cb_owner"

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

// view is what an owner's watchers last saw of one machine.
type view struct {
	owner string
	key   string
}

// entry is the backend's record of one machine: its claim row and what the backend knows beyond it.
type entry struct {
	id    string
	owner string
	image string
	// created stands in for the claim time until the claim row exists.
	created time.Time
	row     *row
	// rowAt is when this process first saw the row's sandbox, so a claim list taken before it cannot drop the machine.
	rowAt     time.Time
	ownerAddr string

	op    string
	opErr string
	// opUntil is when op's process gives up on it, so a later reader takes it as ended.
	opUntil  time.Time
	revision int64
	changed  chan struct{}

	meta map[string]string
	// env maps each env name the claim holds to the fingerprint of the entry this process wrote, "" when unknown.
	env    map[string]string
	wakes  int
	routed bool
	// ready means the agent answered on its port since the claim, a wake or the last restart.
	ready         bool
	readyRestarts int
	mirrored      *cluster.Machine
	// version is the shared row version the entry was loaded from.
	version int64
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
	return e.op != "" && (e.opUntil.IsZero() || now.Before(e.opUntil))
}

// begin marks e as running op from now on, as a new revision, held for lease; zero holds it until it ends.
func (e *entry) begin(op string, now time.Time, lease time.Duration) {
	e.op, e.opErr, e.opUntil = op, "", time.Time{}
	if lease > 0 {
		e.opUntil = now.Add(lease)
	}
	e.revision++
}

func (e *entry) notifyLocked() {
	if e.changed != nil {
		close(e.changed)
		e.changed = nil
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

// load takes rec's fields; the watchers and the ledger copy of e stay.
func (e *entry) load(rec cluster.Machine) {
	e.owner, e.image, e.created, e.ownerAddr = rec.Owner, rec.Image, rec.CreatedAt, rec.OwnerAddr
	e.op, e.opErr, e.opUntil, e.revision = rec.Op, rec.OpErr, rec.OpUntil, rec.Revision
	e.meta, e.wakes, e.routed, e.ready, e.readyRestarts = maps.Clone(rec.Meta), rec.Wakes, rec.Routed, rec.Ready, rec.ReadyRestarts
	e.env = maps.Clone(rec.Env)
	if e.meta == nil {
		e.meta = map[string]string{}
	}
	e.row, e.version = nil, rec.Version
	if rec.Claim != nil {
		var r row
		if json.Unmarshal(rec.Claim, &r) == nil {
			e.row = &r
		}
	}
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
