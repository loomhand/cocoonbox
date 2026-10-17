package boxes

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/cluster"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
)

const (
	operationTimeout   = 10 * time.Minute
	stateTimeout       = time.Hour
	operationRetention = time.Hour
	sharedTimeout      = 5 * time.Second
	// opLease bounds how long a shared operation outlives its process, which renews it every opRenew while it runs.
	opLease = time.Minute
	opRenew = 20 * time.Second
)

var ended = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// SharedOperations is the operation table api processes share.
type SharedOperations interface {
	Operation(ctx context.Context, id string) (cluster.Operation, error)
	Operations(ctx context.Context, since time.Time) ([]cluster.Operation, error)
	PutRunningOperation(ctx context.Context, op cluster.Operation) error
	FinishOperation(ctx context.Context, id string, edit func(events []byte) ([]byte, error)) (bool, error)
}

type eventRecord struct {
	Phase      int32  `json:"phase"`
	Detail     string `json:"detail,omitempty"`
	OffsetKey  string `json:"offset_key"`
	AtMs       int64  `json:"at_ms"`
	BoxState   int32  `json:"box_state,omitzero"`
	DoneBytes  uint64 `json:"done_bytes,omitzero"`
	TotalBytes uint64 `json:"total_bytes,omitzero"`
}

type operationMirror func(cluster.Operation)

type observation struct {
	state  controlv1.BoxState
	detail string
	reason string
}

type operation struct {
	id    string
	user  string
	boxID string
	phase controlv1.Phase
	// park marks the sweep's save and removal of a hibernated machine, which any client call supersedes.
	park     bool
	gen      int64
	ctx      context.Context
	stop     context.CancelFunc
	watchers *watchers
	mirror   operationMirror
	created  time.Time
	expires  time.Time
	// lease, with a shared table, is when other processes take the operation as gone unless this one renews it.
	lease time.Time
	// remote marks a read-only copy of an operation another api process runs.
	remote bool

	mu         sync.Mutex
	events     []*controlv1.OperationEvent
	last       observation
	finishedAt time.Time
	changed    chan struct{}
}

func (o *operation) record(obs observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.finishedAt.IsZero() || obs == o.last {
		return
	}
	o.last = obs
	switch {
	case obs.state == o.target():
		o.appendLocked(controlv1.Phase_PHASE_DONE, obs.detail, obs.state)
	case obs.state == controlv1.BoxState_BOX_STATE_FAILED:
		o.appendLocked(controlv1.Phase_PHASE_FAILED, cmp.Or(obs.reason, obs.detail), obs.state)
	default:
		o.appendLocked(o.phase, obs.detail, obs.state)
	}
}

func (o *operation) note(phase controlv1.Phase, state controlv1.BoxState, detail string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	obs := observation{state: state, detail: detail}
	if !o.finishedAt.IsZero() || obs == o.last {
		return
	}
	o.last = obs
	o.appendLocked(phase, detail, state)
}

// progress appends a transfer event carrying byte counts.
func (o *operation) progress(phase controlv1.Phase, detail string, done, total uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.finishedAt.IsZero() {
		return
	}
	o.last = observation{state: controlv1.BoxState_BOX_STATE_RUNNING, detail: detail}
	o.appendBytesLocked(phase, detail, o.last.state, done, total)
}

func (o *operation) succeed(detail string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finishedAt.IsZero() {
		o.appendLocked(controlv1.Phase_PHASE_DONE, detail, o.last.state)
	}
}

func (o *operation) fail(detail string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finishedAt.IsZero() {
		o.appendLocked(controlv1.Phase_PHASE_FAILED, detail, o.last.state)
	}
}

// since returns the events whose offset key is past offset, whether the log is complete, and a channel closed on the next append.
func (o *operation) since(offset int) ([]*controlv1.OperationEvent, bool, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var events []*controlv1.OperationEvent
	if i := slices.IndexFunc(o.events, func(ev *controlv1.OperationEvent) bool { return offsetOf(ev.GetOffsetKey()) > offset }); i >= 0 {
		events = o.events[i:]
	}
	return events, !o.finishedAt.IsZero(), o.changed
}

func (o *operation) finished() (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.finishedAt, !o.finishedAt.IsZero()
}

func (o *operation) target() controlv1.BoxState {
	if o.phase == controlv1.Phase_PHASE_SNAPSHOTTING {
		return controlv1.BoxState_BOX_STATE_HIBERNATED
	}
	return controlv1.BoxState_BOX_STATE_RUNNING
}

func (o *operation) appendLocked(phase controlv1.Phase, detail string, state controlv1.BoxState) {
	o.appendBytesLocked(phase, detail, state, 0, 0)
}

func (o *operation) appendBytesLocked(phase controlv1.Phase, detail string, state controlv1.BoxState, done, total uint64) {
	next := 1
	if n := len(o.events); n > 0 {
		next = offsetOf(o.events[n-1].GetOffsetKey()) + 1
	}
	o.events = append(o.events, &controlv1.OperationEvent{
		OperationId: o.id,
		Phase:       phase,
		Detail:      detail,
		OffsetKey:   strconv.Itoa(next),
		AtMs:        time.Now().UnixMilli(),
		BoxState:    state,
		DoneBytes:   done,
		TotalBytes:  total,
	})
	if phase == controlv1.Phase_PHASE_DONE || phase == controlv1.Phase_PHASE_FAILED {
		o.finishedAt = time.Now()
		o.stop()
	}
	close(o.changed)
	o.changed = make(chan struct{})
	o.watchers.wake(o.user)
	o.mirrorLocked()
}

func (o *operation) recordLocked() cluster.Operation {
	events := eventRecords(collapseTransfers(o.events))
	return cluster.Operation{
		ID: o.id, User: o.user, Box: o.boxID, Phase: int32(o.phase), Park: o.park, Gen: o.gen, Events: events,
		FinishedAt: o.finishedAt, ExpiresAt: cmp.Or(o.lease, o.expires), CreatedAt: o.created,
	}
}

// gone reports whether o is another process's operation whose lease ran out more than after ago.
func (o *operation) gone(after time.Duration) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.remote && time.Since(o.lease) >= after
}

func (o *operation) leaseEnd() time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lease
}

// adoptLocked takes the events of rec, which another process wrote: the progress of a remote operation or the end of this one.
func (o *operation) adoptLocked(rec cluster.Operation) {
	o.events, o.finishedAt, o.lease = fromEventRecords(rec.ID, rec.Events), rec.FinishedAt, rec.ExpiresAt
	if n := len(o.events); n > 0 {
		o.last = observation{state: o.events[n-1].GetBoxState(), detail: o.events[n-1].GetDetail()}
	}
	o.stop()
	close(o.changed)
	o.changed = make(chan struct{})
	o.watchers.wake(o.user)
}

func (o *operation) mirrorLocked() {
	if o.mirror != nil {
		o.mirror(o.recordLocked())
	}
}

type operations struct {
	watchers *watchers
	mirror   operationMirror
	shared   SharedOperations

	mu   sync.Mutex
	byID map[string]*operation
	// active maps a box to its newest operation, this process's or a read-only copy of another's.
	active map[string]*operation
}

func newOperations() *operations {
	return &operations{watchers: &watchers{next: map[string]chan struct{}{}}, byID: map[string]*operation{}, active: map[string]*operation{}}
}

// start returns the running operation of the same phase on boxID, or supersedes whatever runs there with a new one of user whose context derives from base.
func (r *operations) start(base context.Context, user, boxID string, phase controlv1.Phase, gen int64, timeout time.Duration) (*operation, bool) {
	return r.begin(base, &operation{user: user, boxID: boxID, phase: phase, gen: gen}, timeout)
}

// begin returns the running operation of op's phase and kind on its box, or supersedes whatever runs there with op, whose context derives from base.
func (r *operations) begin(base context.Context, op *operation, timeout time.Duration) (*operation, bool) {
	r.mu.Lock()
	r.sweepLocked()
	cur := r.activeLocked(op.boxID)
	if cur != nil && cur.phase == op.phase && cur.park == op.park {
		r.mu.Unlock()
		return cur, false
	}
	op.id, op.changed, op.watchers, op.mirror, op.created = rand.Text(), make(chan struct{}), r.watchers, r.mirror, time.Now()
	op.expires = op.created.Add(timeout)
	if r.shared != nil {
		op.lease = op.created.Add(opLease)
	}
	op.ctx, op.stop = context.WithDeadline(base, op.expires)
	if cur != nil && !cur.remote {
		cur.fail("superseded by operation " + op.id)
	}
	r.byID[op.id] = op
	r.active[op.boxID] = op
	r.watchers.wake(op.user)
	op.mu.Lock()
	op.mirrorLocked()
	op.mu.Unlock()
	r.mu.Unlock()
	if cur != nil && cur.remote {
		r.end(base, cur.id, "superseded by operation "+op.id)
	}
	return op, true
}

// get returns operation id, from the shared table when this process does not know it; nil when there is none.
func (r *operations) get(ctx context.Context, id string) (*operation, error) {
	if op := r.local(id); op != nil || r.shared == nil {
		return op, nil
	}
	rec, err := r.shared.Operation(ctx, id)
	switch {
	case errors.Is(err, cluster.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if op := r.takeLocked(rec); op != nil {
		return op, nil
	}
	return r.remoteOperation(rec), nil
}

func (r *operations) local(id string) *operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

func (r *operations) runsHere(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	op := r.byID[id]
	return op != nil && !op.remote
}

// all returns the operations this process runs or ran.
func (r *operations) all() []*operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*operation
	for _, op := range r.byID {
		if !op.remote {
			out = append(out, op)
		}
	}
	return out
}

// activeFor returns the unfinished operation on boxID, this process's or another's, nil when none runs.
func (r *operations) activeFor(boxID string) *operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeLocked(boxID)
}

func (r *operations) activeLocked(boxID string) *operation {
	op := r.active[boxID]
	if op == nil {
		return nil
	}
	if _, done := op.finished(); done || op.gone(0) {
		return nil
	}
	return op
}

// notified takes in the change of operation id: a remote one starts, progresses or ends, or a local one another process ended.
func (r *operations) notified(ctx context.Context, id string) {
	rec, err := r.shared.Operation(ctx, id)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case errors.Is(err, cluster.ErrNotFound):
		if op := r.byID[id]; op != nil && op.remote {
			r.forgetLocked(op)
		}
	case err != nil:
		log.WithFunc("boxes.operations.notified").Warnf(ctx, "read operation %s: %v", id, err)
	default:
		r.takeLocked(rec)
	}
}

// reload takes the shared table in again, since notifies sent while none listened are lost.
func (r *operations) reload(ctx context.Context) error {
	recs, err := r.shared.Operations(ctx, time.Now().Add(-operationRetention))
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	listed := map[string]bool{}
	for _, rec := range recs {
		listed[rec.ID] = true
		r.takeLocked(rec)
	}
	for id, op := range r.byID {
		if op.remote && !listed[id] {
			r.forgetLocked(op)
		}
	}
	return nil
}

// takeLocked takes rec into the copy of another process's operation, or into this process's operation another one ended, and returns the operation; nil for an ended one this process never followed.
func (r *operations) takeLocked(rec cluster.Operation) *operation {
	op := r.byID[rec.ID]
	switch {
	case op == nil && (!rec.FinishedAt.IsZero() || !time.Now().Before(rec.ExpiresAt)):
		return nil
	case op == nil:
		op = r.remoteOperation(rec)
		r.byID[op.id] = op
	default:
		op.mu.Lock()
		if op.remote || op.finishedAt.IsZero() && !rec.FinishedAt.IsZero() {
			op.adoptLocked(rec)
		}
		op.mu.Unlock()
	}
	if cur := r.active[op.boxID]; cur == nil || !cur.created.After(op.created) {
		r.active[op.boxID] = op
	}
	r.watchers.wake(op.user)
	return op
}

func (r *operations) forgetLocked(op *operation) {
	delete(r.byID, op.id)
	if r.active[op.boxID] == op {
		delete(r.active, op.boxID)
	}
}

// end finishes operation id in the shared table with a failure that says detail, so its process stops it.
func (r *operations) end(ctx context.Context, id, detail string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedTimeout)
	defer cancel()
	if _, err := r.shared.FinishOperation(ctx, id, func(raw []byte) ([]byte, error) {
		var events []eventRecord
		if err := json.Unmarshal(raw, &events); err != nil {
			return nil, fmt.Errorf("decode the events of %s: %w", id, err)
		}
		var state int32
		next := 1
		if n := len(events); n > 0 {
			state, next = events[n-1].BoxState, offsetOf(events[n-1].OffsetKey)+1
		}
		events = append(events, eventRecord{
			Phase: int32(controlv1.Phase_PHASE_FAILED), Detail: detail, OffsetKey: strconv.Itoa(next), AtMs: time.Now().UnixMilli(), BoxState: state,
		})
		return json.Marshal(events)
	}); err != nil {
		log.WithFunc("boxes.operations.end").Warnf(ctx, "end operation %s in the shared table: %v", id, err)
	}
}

// renew extends the leases of this process's operations until ctx ends, then ends them in the shared table.
func (r *operations) renew(ctx context.Context) {
	ticker := time.NewTicker(opRenew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			for _, op := range r.running() {
				r.end(ctx, op.id, "the api process running the operation stopped")
			}
			return
		case <-ticker.C:
		}
		for _, op := range r.running() {
			op.mu.Lock()
			if op.finishedAt.IsZero() {
				op.lease = time.Now().Add(opLease)
				op.mirrorLocked()
			}
			op.mu.Unlock()
		}
	}
}

func (r *operations) running() []*operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*operation
	for _, op := range r.byID {
		if _, done := op.finished(); !done && !op.remote {
			out = append(out, op)
		}
	}
	return out
}

// remoteOperation is a read-only copy of rec; nothing it records leaves this process.
func (r *operations) remoteOperation(rec cluster.Operation) *operation {
	op := &operation{
		id: rec.ID, user: rec.User, boxID: rec.Box, phase: controlv1.Phase(rec.Phase), park: rec.Park, gen: rec.Gen, ctx: ended, stop: func() {},
		watchers: r.watchers, created: rec.CreatedAt, remote: true, changed: make(chan struct{}),
	}
	op.adoptLocked(rec)
	return op
}

func (r *operations) sweepLocked() {
	for _, op := range r.byID {
		if at, done := op.finished(); done && time.Since(at) > operationRetention || op.gone(operationRetention) {
			r.forgetLocked(op)
		}
	}
}

// collapseTransfers keeps, of each run of transfer events, the last, so a long upload stores a bounded log.
func collapseTransfers(events []*controlv1.OperationEvent) []*controlv1.OperationEvent {
	out := make([]*controlv1.OperationEvent, 0, len(events))
	for _, ev := range events {
		if n := len(out); n > 0 && transfer(ev) && transfer(out[n-1]) {
			out[n-1] = ev
			continue
		}
		out = append(out, ev)
	}
	return out
}

func transfer(ev *controlv1.OperationEvent) bool {
	return ev.GetTotalBytes() > 0
}

func offsetOf(key string) int {
	n, _ := strconv.Atoi(key)
	return n
}
