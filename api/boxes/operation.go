package boxes

import (
	"cmp"
	"crypto/rand"
	"fmt"
	"strconv"
	"sync"
	"time"

	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

const (
	operationTimeout   = 10 * time.Minute
	operationRetention = time.Hour
)

type operation struct {
	id    string
	boxID string
	phase controlv1.Phase
	gen   int64

	mu         sync.Mutex
	events     []*controlv1.OperationEvent
	last       observation
	finishedAt time.Time
	changed    chan struct{}
	timer      *time.Timer
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
		o.timer.Stop()
	}
	close(o.changed)
	o.changed = make(chan struct{})
}

type operations struct {
	mu     sync.Mutex
	byID   map[string]*operation
	active map[string]*operation
}

func newOperations() *operations {
	return &operations{byID: map[string]*operation{}, active: map[string]*operation{}}
}

// start returns the running operation of the same phase on boxID, or supersedes whatever runs there with a new one.
func (r *operations) start(boxID string, phase controlv1.Phase, gen int64) (*operation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()
	if cur := r.active[boxID]; cur != nil {
		if _, done := cur.finished(); !done && cur.phase == phase {
			return cur, false
		}
	}
	op := &operation{id: rand.Text(), boxID: boxID, phase: phase, gen: gen, changed: make(chan struct{})}
	op.mu.Lock()
	op.timer = time.AfterFunc(operationTimeout, func() { op.fail(fmt.Sprintf("timed out after %s", operationTimeout)) })
	op.mu.Unlock()
	if cur := r.active[boxID]; cur != nil {
		cur.fail("superseded by operation " + op.id)
	}
	r.byID[op.id] = op
	r.active[boxID] = op
	return op, true
}

func (r *operations) get(id string) *operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

func (r *operations) activeFor(boxID string) *operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[boxID]
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
