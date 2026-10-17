package cluster

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const machineColumns = `id, owner, image, created_at, claim, owner_addr, op, op_err, op_until, revision, meta, wakes, routed,
	ready, ready_restarts, env, version`

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

// Equal reports whether m and o record the same machine, Version aside.
func (m Machine) Equal(o Machine) bool {
	return m.ID == o.ID && m.Owner == o.Owner && m.Image == o.Image && sameTime(m.CreatedAt, o.CreatedAt) && string(m.Claim) == string(o.Claim) &&
		m.OwnerAddr == o.OwnerAddr && m.Op == o.Op && m.OpErr == o.OpErr && sameTime(m.OpUntil, o.OpUntil) && m.Revision == o.Revision &&
		maps.Equal(m.Meta, o.Meta) && m.Wakes == o.Wakes && m.Routed == o.Routed && m.Ready == o.Ready && m.ReadyRestarts == o.ReadyRestarts &&
		maps.Equal(m.Env, o.Env)
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
		if _, err := tx.Exec(ctx, `INSERT INTO machines (id, owner, image, created_at, claim, owner_addr, op, op_err, op_until, revision, meta, wakes,
			routed, ready, ready_restarts, env) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			ON CONFLICT (id) DO UPDATE SET owner = $2, image = $3, created_at = $4, claim = $5, owner_addr = $6, op = $7, op_err = $8,
			op_until = $9, revision = $10, meta = $11, wakes = $12, routed = $13, ready = $14, ready_restarts = $15, env = $16,
			version = nextval('machines_version'), updated_at = now()`, machineArgs(m)...); err != nil {
			return fmt.Errorf("put machine %s: %w", m.ID, err)
		}
		return notify(ctx, tx, ChannelMachines, m.ID)
	})
}

// UpdateMachine writes edit's change of machine id on the version read, rerunning edit after a concurrent write; edit returns false to write nothing.
func (s *Store) UpdateMachine(ctx context.Context, id string, edit func(m *Machine, found bool) (bool, error)) (Machine, error) {
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
		return notify(ctx, tx, ChannelMachines, id)
	})
}

// DeleteMachineIf removes machine id while cond holds, rerunning cond after a concurrent write; ErrNotFound when there is none.
func (s *Store) DeleteMachineIf(ctx context.Context, id string, cond func(m Machine) bool) (bool, error) {
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
		return notify(ctx, tx, ChannelMachines, id)
	})
	return removed, err
}

// writeMachine inserts m when it was not found, else updates it while it keeps m.Version; ErrStale when another write came first.
func (s *Store) writeMachine(ctx context.Context, m Machine, found bool) (Machine, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			cmd pgconn.CommandTag
			err error
		)
		if found {
			cmd, err = tx.Exec(ctx, `UPDATE machines SET owner = $2, image = $3, created_at = $4, claim = $5, owner_addr = $6, op = $7, op_err = $8,
				op_until = $9, revision = $10, meta = $11, wakes = $12, routed = $13, ready = $14, ready_restarts = $15, env = $16,
				version = nextval('machines_version'), updated_at = now() WHERE id = $1 AND version = $17`, append(machineArgs(m), m.Version)...)
		} else {
			cmd, err = tx.Exec(ctx, `INSERT INTO machines (id, owner, image, created_at, claim, owner_addr, op, op_err, op_until, revision, meta,
				wakes, routed, ready, ready_restarts, env) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
				ON CONFLICT (id) DO NOTHING`, machineArgs(m)...)
		}
		switch {
		case err != nil:
			return err
		case cmd.RowsAffected() == 0:
			return ErrStale
		}
		return notify(ctx, tx, ChannelMachines, m.ID)
	})
	if errors.Is(err, ErrStale) {
		return Machine{}, ErrStale
	}
	if err != nil {
		return Machine{}, fmt.Errorf("write machine %s: %w", m.ID, err)
	}
	return s.Machine(ctx, m.ID)
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

// sameTime compares at the microsecond PostgreSQL keeps.
func sameTime(a, b time.Time) bool {
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

func notify(ctx context.Context, tx pgx.Tx, channel, payload string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload); err != nil {
		return fmt.Errorf("notify %s: %w", channel, err)
	}
	return nil
}
