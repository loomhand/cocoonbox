// Package machines defines the interface between the cocoonbox services and a machine backend.
package machines

import (
	"context"
	"errors"
	"time"
)

const (
	StateStarting State = iota + 1
	StateRunning
	StateHibernating
	StateHibernated
	StateFailed
	// StateGone reports that a watched machine was deleted.
	StateGone

	ActionNone    Action = 0
	ActionCreated Action = 1
	ActionWoken   Action = 2
)

var (
	ErrNotFound = errors.New("machine not found")
	ErrDeleting = errors.New("machine is being deleted")
	// ErrNoAddress reports a running machine whose address is not yet known.
	ErrNoAddress = errors.New("machine has no address yet")
	// ErrConflict reports a concurrent change; the caller may retry.
	ErrConflict = errors.New("concurrent machine change")
)

// State is the lifecycle state of a machine.
type State int

// Action is what Ensure did to reach its result.
type Action int

// Flavor sizes a machine.
type Flavor struct {
	VCPUs     uint32
	MemoryMiB uint32
	DiskGiB   uint32
}

// Spec describes a machine to create; Owner is an opaque key the backend stores and filters on.
type Spec struct {
	ID     string
	Owner  string
	Flavor Flavor
	// Image overrides the backend's default image.
	Image string
}

// Machine is a backend's view of one machine.
type Machine struct {
	ID     string
	Owner  string
	State  State
	Image  string
	Flavor Flavor
	// Reason explains StateFailed.
	Reason string
	// Detail summarizes the backend's raw status for logs; it may name internal hosts.
	Detail    string
	Waiting   bool
	CreatedAt time.Time
	// Revision increases with every lifecycle request; Watch judges state no older than it.
	Revision int64
	// Incarnation changes whenever the machine boots anew: first boot, wake, rebuild.
	Incarnation string
	// Meta holds the service's small key/value records stored with the machine.
	Meta map[string]string
}

// Identity tells a box agent which box and wake it serves and which edge key signs its credentials.
type Identity struct {
	BoxID     string `json:"box_id"`
	WakeID    string `json:"wake_id"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// NotRunningError is returned by Address for a machine that cannot take traffic.
type NotRunningError struct {
	State State
}

func (e *NotRunningError) Error() string { return "machine is not running" }

// Machines is implemented by every machine backend.
type Machines interface {
	// Ensure creates the machine, or wakes it when hibernated, and returns without waiting.
	Ensure(ctx context.Context, spec Spec) (Machine, Action, error)
	Get(ctx context.Context, id string) (Machine, error)
	// List returns owner's machines that are not being deleted, or every such machine when owner is empty.
	List(ctx context.Context, owner string) ([]Machine, error)
	// Hibernate requests hibernation and returns without waiting.
	Hibernate(ctx context.Context, id string) (Machine, error)
	// Watch sends the machine's state, judged no older than revision, on every change until ctx ends or the machine is gone.
	Watch(ctx context.Context, id string, revision int64) <-chan Machine
	// Address returns host:port of the running machine's agent.
	Address(ctx context.Context, id string) (string, error)
	// Delete removes the machine and its disk.
	Delete(ctx context.Context, id string) error
	// SetMeta stores value under key in the machine's Meta.
	SetMeta(ctx context.Context, id, key, value string) error
	// Deliver hands the running machine's agent its identity and, when gwKey is set, its gw key.
	Deliver(ctx context.Context, id string, identity Identity, gwKey string) error
}
