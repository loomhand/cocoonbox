package cluster

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PutPin keeps snapshot of user's box from every process's gc until DeletePin(id) or until expires.
func (s *Store) PutPin(ctx context.Context, id, user, box, snapshot string, expires time.Time) error {
	if _, err := s.pool.Exec(ctx, "INSERT INTO snapshot_pins (id, user_id, box, snapshot, expires_at) VALUES ($1, $2, $3, $4, $5)",
		id, user, box, snapshot, expires); err != nil {
		return fmt.Errorf("pin snapshot %s: %w", snapshot, err)
	}
	return nil
}

// DeletePin removes pin id.
func (s *Store) DeletePin(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM snapshot_pins WHERE id = $1", id); err != nil {
		return fmt.Errorf("unpin %s: %w", id, err)
	}
	return nil
}

// Pins returns the snapshots of user's box that live pins keep, after it removes the expired pins.
func (s *Store) Pins(ctx context.Context, user, box string) ([]string, error) {
	if _, err := s.pool.Exec(ctx, "DELETE FROM snapshot_pins WHERE expires_at <= now()"); err != nil {
		return nil, fmt.Errorf("purge expired pins: %w", err)
	}
	rows, err := s.pool.Query(ctx, "SELECT snapshot FROM snapshot_pins WHERE user_id = $1 AND box = $2", user, box)
	if err != nil {
		return nil, fmt.Errorf("list pins of %s: %w", box, err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
