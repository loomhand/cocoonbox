package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const operationColumns = "id, user_id, box, phase, park, gen, events, finished_at, expires_at, created_at, releasing"

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
	// ExpiresAt ends the lease its process renews while it runs, so an unfinished one past it ran in a process that is gone.
	ExpiresAt time.Time
	CreatedAt time.Time
	Releasing bool
}

// Operations returns the operations of user, of every user when empty, unfinished or finished after since.
func (s *Store) Operations(ctx context.Context, user string, since time.Time) ([]Operation, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+operationColumns+" FROM operations WHERE ($1 = '' OR user_id = $1) AND (finished_at IS NULL OR finished_at > $2) ORDER BY id",
		user, since)
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
	return s.putOperation(ctx, op, "")
}

// PutRunningOperation writes op unless the stored one already finished, which another process may have done.
func (s *Store) PutRunningOperation(ctx context.Context, op Operation) error {
	return s.putOperation(ctx, op, "WHERE operations.finished_at IS NULL")
}

// FinishOperation ends unfinished, unclaimed operation id with edit's change of its events, rerunning edit after a concurrent write.
func (s *Store) FinishOperation(ctx context.Context, id string, edit func(events []byte) ([]byte, error)) (bool, error) {
	for {
		op, err := s.Operation(ctx, id)
		if err != nil || !op.FinishedAt.IsZero() || op.Releasing {
			return false, err
		}
		events, err := edit(op.Events)
		if err != nil {
			return false, err
		}
		ended, err := s.finishOn(ctx, id, op.Events, events)
		if err != nil || ended {
			return ended, err
		}
	}
}

// ClaimOperation marks unfinished operation id releasing, so FinishOperation no longer ends it, and reports whether it did.
func (s *Store) ClaimOperation(ctx context.Context, id string) (bool, error) {
	cmd, err := s.pool.Exec(ctx, "UPDATE operations SET releasing = true WHERE id = $1 AND finished_at IS NULL AND NOT releasing", id)
	if err != nil {
		return false, fmt.Errorf("claim operation %s: %w", id, err)
	}
	return cmd.RowsAffected() == 1, nil
}

// PurgeOperations removes the operations finished, or expired unfinished, before before.
func (s *Store) PurgeOperations(ctx context.Context, before time.Time) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM operations WHERE finished_at < $1 OR finished_at IS NULL AND expires_at < $1", before); err != nil {
		return fmt.Errorf("purge operations: %w", err)
	}
	return nil
}

// finishOn ends operation id with events while its events are still old.
func (s *Store) finishOn(ctx context.Context, id string, old, events []byte) (bool, error) {
	var ended bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, "UPDATE operations SET events = $3, finished_at = now() WHERE id = $1 AND finished_at IS NULL AND NOT releasing AND events::text = $2",
			id, string(old), string(events))
		if err != nil {
			return fmt.Errorf("finish operation %s: %w", id, err)
		}
		if ended = cmd.RowsAffected() == 1; !ended {
			return nil
		}
		return notifyChange(ctx, tx, ChannelOperations, Change{ID: id, Finished: true})
	})
	return ended, err
}

func (s *Store) putOperation(ctx context.Context, op Operation, cond string) error {
	var finished *time.Time
	if !op.FinishedAt.IsZero() {
		finished = &op.FinishedAt
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO operations (id, user_id, box, phase, park, gen, events, finished_at, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (id) DO UPDATE SET events = $7, finished_at = $8, expires_at = $9 `+cond,
			op.ID, op.User, op.Box, op.Phase, op.Park, op.Gen, string(op.Events), finished, op.ExpiresAt, op.CreatedAt); err != nil {
			return fmt.Errorf("put operation %s: %w", op.ID, err)
		}
		return notifyChange(ctx, tx, ChannelOperations, Change{ID: op.ID, Finished: !op.FinishedAt.IsZero()})
	})
}

func scanOperation(row pgx.CollectableRow) (Operation, error) {
	var op Operation
	var events string
	var finished *time.Time
	err := row.Scan(&op.ID, &op.User, &op.Box, &op.Phase, &op.Park, &op.Gen, &events, &finished, &op.ExpiresAt, &op.CreatedAt, &op.Releasing)
	op.Events = []byte(events)
	if finished != nil {
		op.FinishedAt = *finished
	}
	return op, err
}
