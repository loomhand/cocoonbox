package boxes

import (
	"cmp"
	"context"
	"crypto/rand"
	"strconv"
	"sync"
	"time"

	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
)

const (
	operationTimeout   = 10 * time.Minute
	stateTimeout       = time.Hour
	operationRetention = time.Hour
)

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

// note appends a progress event unless it repeats the previous one.
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
	o.appendLocked(phase, detail, o.last.state)
	ev := o.events[len(o.events)-1]
	ev.DoneBytes, ev.TotalBytes = done, total
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

// since returns the events after offset, whether the log is complete, and a channel closed on the next append.
func (o *operation) since(offset int) ([]*controlv1.OperationEvent, bool, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var events []*controlv1.OperationEvent
	if offset < len(o.events) {
		events = o.events[offset:]
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
	o.events = append(o.events, &controlv1.OperationEvent{
		OperationId: o.id,
		Phase:       phase,
		Detail:      detail,
		OffsetKey:   strconv.Itoa(len(o.events) + 1),
		AtMs:        time.Now().UnixMilli(),
		BoxState:    state,
	})
	if phase == controlv1.Phase_PHASE_DONE || phase == controlv1.Phase_PHASE_FAILED {
		o.finishedAt = time.Now()
		o.stop()
	}
	close(o.changed)
	o.changed = make(chan struct{})
	o.watchers.wake(o.user)
}

type operations struct {
	watchers *watchers

	mu     sync.Mutex
	byID   map[string]*operation
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
	defer r.mu.Unlock()
	r.sweepLocked()
	if cur := r.active[op.boxID]; cur != nil {
		if _, done := cur.finished(); !done && cur.phase == op.phase && cur.park == op.park {
			return cur, false
		}
	}
	op.id, op.changed, op.watchers = rand.Text(), make(chan struct{}), r.watchers
	op.ctx, op.stop = context.WithTimeout(base, timeout)
	if cur := r.active[op.boxID]; cur != nil {
		cur.fail("superseded by operation " + op.id)
	}
	r.byID[op.id] = op
	r.active[op.boxID] = op
	r.watchers.wake(op.user)
	return op, true
}

func (r *operations) get(id string) *operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

// activeFor returns the unfinished operation on boxID, nil when none runs.
func (r *operations) activeFor(boxID string) *operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	op := r.active[boxID]
	if op == nil {
		return nil
	}
	if _, done := op.finished(); done {
		return nil
	}
	return op
}

func (r *operations) sweepLocked() {
	for id, op := range r.byID {
		if at, done := op.finished(); done && time.Since(at) > operationRetention {
			delete(r.byID, id)
			if r.active[op.boxID] == op {
				delete(r.active, op.boxID)
			}
		}
	}
}
