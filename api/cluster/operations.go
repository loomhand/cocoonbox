package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const operationColumns = "id, user_id, box, phase, park, gen, events, finished_at, expires_at, created_at"

// Operation is one box operation and the events it logged, encoded by the service.
type Operation struct {
	ID         string
	User       string
	Box        string
	Phase      int32
	Park       bool
	Gen        int64
	Events     []byte
	FinishedAt time.Time
	// ExpiresAt is when the process running it gives up, so an unfinished one past it ran in a process that is gone.
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Operations returns the operations unfinished or finished after since.
func (s *Store) Operations(ctx context.Context, since time.Time) ([]Operation, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+operationColumns+" FROM operations WHERE finished_at IS NULL OR finished_at > $1 ORDER BY id", since)
	if err != nil {
		return nil, fmt.Errorf("list operations: %w", err)
	}
	return pgx.CollectRows(rows, scanOperation)
}

// Operation returns operation id, ErrNotFound when there is none.
func (s *Store) Operation(ctx context.Context, id string) (Operation, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+operationColumns+" FROM operations WHERE id = $1", id)
	if err != nil {
		return Operation{}, fmt.Errorf("read operation %s: %w", id, err)
	}
	op, err := pgx.CollectExactlyOneRow(rows, scanOperation)
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	return op, err
}

// PutOperation writes op whatever is stored.
func (s *Store) PutOperation(ctx context.Context, op Operation) error {
	var finished *time.Time
	if !op.FinishedAt.IsZero() {
		finished = &op.FinishedAt
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO operations (id, user_id, box, phase, park, gen, events, finished_at, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (id) DO UPDATE SET events = $7, finished_at = $8`,
			op.ID, op.User, op.Box, op.Phase, op.Park, op.Gen, string(op.Events), finished, op.ExpiresAt, op.CreatedAt); err != nil {
			return fmt.Errorf("put operation %s: %w", op.ID, err)
		}
		return notify(ctx, tx, ChannelOperations, op.ID)
	})
}

// PurgeOperations removes the operations finished, or expired unfinished, before before.
func (s *Store) PurgeOperations(ctx context.Context, before time.Time) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM operations WHERE finished_at < $1 OR finished_at IS NULL AND expires_at < $1", before); err != nil {
		return fmt.Errorf("purge operations: %w", err)
	}
	return nil
}

func scanOperation(row pgx.CollectableRow) (Operation, error) {
	var op Operation
	var events string
	var finished *time.Time
	err := row.Scan(&op.ID, &op.User, &op.Box, &op.Phase, &op.Park, &op.Gen, &events, &finished, &op.ExpiresAt, &op.CreatedAt)
	op.Events = []byte(events)
	if finished != nil {
		op.FinishedAt = *finished
	}
	return op, err
}
