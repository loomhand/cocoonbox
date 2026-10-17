package cluster

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PutActivity records when a client last reached each box, keeping a later time another process wrote.
func (s *Store) PutActivity(ctx context.Context, reached map[string]time.Time) error {
	if len(reached) == 0 {
		return nil
	}
	boxes, ats := make([]string, 0, len(reached)), make([]time.Time, 0, len(reached))
	for box, at := range reached {
		boxes, ats = append(boxes, box), append(ats, at)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO box_activity (box, last_at) SELECT * FROM unnest($1::text[], $2::timestamptz[])
		ON CONFLICT (box) DO UPDATE SET last_at = greatest(box_activity.last_at, EXCLUDED.last_at)`, boxes, ats); err != nil {
		return fmt.Errorf("put box activity: %w", err)
	}
	return nil
}

// Activity returns when a client last reached each box reached after since.
func (s *Store) Activity(ctx context.Context, since time.Time) (map[string]time.Time, error) {
	rows, err := s.pool.Query(ctx, "SELECT box, last_at FROM box_activity WHERE last_at > $1", since)
	if err != nil {
		return nil, fmt.Errorf("read box activity: %w", err)
	}
	out := map[string]time.Time{}
	var box string
	var at time.Time
	if _, err = pgx.ForEachRow(rows, []any{&box, &at}, func() error {
		out[box] = at
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read box activity: %w", err)
	}
	return out, nil
}

// PurgeActivity removes the activity older than before.
func (s *Store) PurgeActivity(ctx context.Context, before time.Time) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM box_activity WHERE last_at < $1", before); err != nil {
		return fmt.Errorf("purge box activity: %w", err)
	}
	return nil
}
