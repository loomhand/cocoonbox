package cluster

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
)

const machineColumns = `id, owner, image, created_at, claim, owner_addr, op, op_err, op_until, revision, meta, wakes, routed,
	ready, ready_restarts, env, version`

// MachineEdit changes machine m, found false for none, and returns false to write nothing.
type MachineEdit func(m *Machine, found bool) (bool, error)

// MachineCond decides whether machine m may go.
type MachineCond func(m Machine) bool

// Machine is a machine backend's record of one machine; Claim is the backend's own claim row, nil until it exists.
type Machine struct {
	ID        string
	Owner     string
	Image     string
	CreatedAt time.Time
	Claim     []byte
	OwnerAddr string

	Op       string
	OpErr    string
	OpUntil  time.Time
	Revision int64

	Meta          map[string]string
	Wakes         int
	Routed        bool
	Ready         bool
	ReadyRestarts int
	// Env maps each env name of the claim to the fingerprint of the value written, "" when unknown.
	Env map[string]string

	// Version increases with every write, across rows; UpdateMachine writes only on the version it read.
	Version int64
}

// Equal reports whether m and o record the same machine, Version aside, every field compared as PostgreSQL keeps it.
func (m Machine) Equal(o Machine) bool {
	return reflect.DeepEqual(m.stored(), o.stored())
}

// stored is m as a read of its row returns it: times at the microsecond, empty bytes and maps as nil, no version.
func (m Machine) stored() Machine {
	m.CreatedAt, m.OpUntil, m.Version = microseconds(m.CreatedAt), microseconds(m.OpUntil), 0
	if len(m.Claim) == 0 {
		m.Claim = nil
	}
	if len(m.Meta) == 0 {
		m.Meta = nil
	}
	if len(m.Env) == 0 {
		m.Env = nil
	}
	return m
}

// Machines returns every machine.
func (s *Store) Machines(ctx context.Context) ([]Machine, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+machineColumns+" FROM machines ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	return pgx.CollectRows(rows, scanMachine)
}

// Machine returns machine id, ErrNotFound when there is none.
func (s *Store) Machine(ctx context.Context, id string) (Machine, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+machineColumns+" FROM machines WHERE id = $1", id)
	if err != nil {
		return Machine{}, fmt.Errorf("read machine %s: %w", id, err)
	}
	m, err := pgx.CollectExactlyOneRow(rows, scanMachine)
	if errors.Is(err, pgx.ErrNoRows) {
		return Machine{}, ErrNotFound
	}
	return m, err
}

// PutMachine writes m whatever the stored version.
func (s *Store) PutMachine(ctx context.Context, m Machine) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var version int64
		if err := tx.QueryRow(ctx, `INSERT INTO machines (id, owner, image, created_at, claim, owner_addr, op, op_err, op_until, revision, meta, wakes,
			routed, ready, ready_restarts, env) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			ON CONFLICT (id) DO UPDATE SET owner = $2, image = $3, created_at = $4, claim = $5, owner_addr = $6, op = $7, op_err = $8,
			op_until = $9, revision = $10, meta = $11, wakes = $12, routed = $13, ready = $14, ready_restarts = $15, env = $16,
			version = nextval('machines_version'), updated_at = now() RETURNING version`, machineArgs(m)...).Scan(&version); err != nil {
			return fmt.Errorf("put machine %s: %w", m.ID, err)
		}
		return notifyChange(ctx, tx, ChannelMachines, Change{ID: m.ID, Version: version})
	})
}

// UpdateMachine writes edit's change of machine id on the version read, rerunning edit after a concurrent write; edit returns false to write nothing.
func (s *Store) UpdateMachine(ctx context.Context, id string, edit MachineEdit) (Machine, error) {
	for {
		m, err := s.Machine(ctx, id)
		found := err == nil
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Machine{}, err
		}
		if !found {
			m = Machine{ID: id}
		}
		write, err := edit(&m, found)
		if err != nil || !write {
			return m, err
		}
		m.ID = id
		next, err := s.writeMachine(ctx, m, found)
		if !errors.Is(err, ErrStale) {
			return next, err
		}
	}
}

// DeleteMachine removes machine id; removing a missing one succeeds.
func (s *Store) DeleteMachine(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM machines WHERE id = $1", id); err != nil {
			return fmt.Errorf("delete machine %s: %w", id, err)
		}
		return notifyChange(ctx, tx, ChannelMachines, Change{ID: id})
	})
}

// DeleteMachineIf removes machine id while cond holds, rerunning cond after a concurrent write; ErrNotFound when there is none.
func (s *Store) DeleteMachineIf(ctx context.Context, id string, cond MachineCond) (bool, error) {
	for {
		m, err := s.Machine(ctx, id)
		if err != nil || !cond(m) {
			return false, err
		}
		if removed, err := s.deleteVersion(ctx, id, m.Version); err != nil || removed {
			return removed, err
		}
	}
}

// deleteVersion removes machine id while it keeps version.
func (s *Store) deleteVersion(ctx context.Context, id string, version int64) (bool, error) {
	var removed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, "DELETE FROM machines WHERE id = $1 AND version = $2", id, version)
		if err != nil {
			return fmt.Errorf("delete machine %s: %w", id, err)
		}
		if removed = cmd.RowsAffected() == 1; !removed {
			return nil
		}
		return notifyChange(ctx, tx, ChannelMachines, Change{ID: id})
	})
	return removed, err
}

// writeMachine inserts m when it was not found, else updates it while it keeps m.Version; ErrStale when another write came first.
func (s *Store) writeMachine(ctx context.Context, m Machine, found bool) (Machine, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if found {
			err = tx.QueryRow(ctx, `UPDATE machines SET owner = $2, image = $3, created_at = $4, claim = $5, owner_addr = $6, op = $7, op_err = $8,
				op_until = $9, revision = $10, meta = $11, wakes = $12, routed = $13, ready = $14, ready_restarts = $15, env = $16,
				version = nextval('machines_version'), updated_at = now() WHERE id = $1 AND version = $17 RETURNING version`,
				append(machineArgs(m), m.Version)...).Scan(&m.Version)
		} else {
			err = tx.QueryRow(ctx, `INSERT INTO machines (id, owner, image, created_at, claim, owner_addr, op, op_err, op_until, revision, meta,
				wakes, routed, ready, ready_restarts, env) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
				ON CONFLICT (id) DO NOTHING RETURNING version`, machineArgs(m)...).Scan(&m.Version)
		}
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrStale
		case err != nil:
			return err
		}
		return notifyChange(ctx, tx, ChannelMachines, Change{ID: m.ID, Version: m.Version})
	})
	if errors.Is(err, ErrStale) {
		return Machine{}, ErrStale
	}
	if err != nil {
		return Machine{}, fmt.Errorf("write machine %s: %w", m.ID, err)
	}
	return m, nil
}

func machineArgs(m Machine) []any {
	var opUntil *time.Time
	if !m.OpUntil.IsZero() {
		opUntil = &m.OpUntil
	}
	var claim any
	if m.Claim != nil {
		claim = string(m.Claim)
	}
	meta, env := m.Meta, m.Env
	if meta == nil {
		meta = map[string]string{}
	}
	if env == nil {
		env = map[string]string{}
	}
	return []any{m.ID, m.Owner, m.Image, m.CreatedAt, claim, m.OwnerAddr, m.Op, m.OpErr, opUntil, m.Revision, meta, m.Wakes, m.Routed, m.Ready, m.ReadyRestarts, env}
}

func scanMachine(row pgx.CollectableRow) (Machine, error) {
	var m Machine
	var claim *string
	var opUntil *time.Time
	err := row.Scan(&m.ID, &m.Owner, &m.Image, &m.CreatedAt, &claim, &m.OwnerAddr, &m.Op, &m.OpErr, &opUntil, &m.Revision, &m.Meta, &m.Wakes,
		&m.Routed, &m.Ready, &m.ReadyRestarts, &m.Env, &m.Version)
	if claim != nil {
		m.Claim = []byte(*claim)
	}
	if opUntil != nil {
		m.OpUntil = *opUntil
	}
	return m, err
}

func microseconds(t time.Time) time.Time {
	return time.UnixMicro(t.UnixMicro()).UTC()
}
