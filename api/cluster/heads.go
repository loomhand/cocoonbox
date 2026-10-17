package cluster

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Head is the stored head of one box, as the state layer encodes it.
type Head struct {
	User    string
	Box     string
	Head    []byte
	Version int64
}

// Heads returns every box head.
func (s *Store) Heads(ctx context.Context) ([]Head, error) {
	rows, err := s.pool.Query(ctx, "SELECT user_id, box, head, version FROM box_heads ORDER BY user_id, box")
	if err != nil {
		return nil, fmt.Errorf("list box heads: %w", err)
	}
	return pgx.CollectRows(rows, scanHead)
}

// Head returns the head of user's box, ErrNotFound when there is none.
func (s *Store) Head(ctx context.Context, user, box string) (Head, error) {
	rows, err := s.pool.Query(ctx, "SELECT user_id, box, head, version FROM box_heads WHERE user_id = $1 AND box = $2", user, box)
	if err != nil {
		return Head{}, fmt.Errorf("read head of %s: %w", box, err)
	}
	h, err := pgx.CollectExactlyOneRow(rows, scanHead)
	if errors.Is(err, pgx.ErrNoRows) {
		return Head{}, ErrNotFound
	}
	return h, err
}

// PutHead writes the head of user's box whatever the stored version, unless the row already holds it.
func (s *Store) PutHead(ctx context.Context, user, box string, head []byte) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var version int64
		err := tx.QueryRow(ctx, `INSERT INTO box_heads (user_id, box, head) VALUES ($1, $2, $3)
			ON CONFLICT (user_id, box) DO UPDATE SET head = $3, version = box_heads.version + 1, updated_at = now()
			WHERE box_heads.head::text IS DISTINCT FROM EXCLUDED.head::text RETURNING version`, user, box, string(head)).Scan(&version)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("put head of %s: %w", box, err)
		}
		return notifyHead(ctx, tx, Head{User: user, Box: box, Head: head, Version: version})
	})
}

// SwapHead writes the head of user's box while it keeps version, 0 for none stored; ErrStale when another write came first.
func (s *Store) SwapHead(ctx context.Context, user, box string, head []byte, version int64) (int64, error) {
	var next int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if version == 0 {
			err = tx.QueryRow(ctx, `INSERT INTO box_heads (user_id, box, head) VALUES ($1, $2, $3) ON CONFLICT (user_id, box) DO NOTHING
				RETURNING version`, user, box, string(head)).Scan(&next)
		} else {
			err = tx.QueryRow(ctx, `UPDATE box_heads SET head = $3, version = version + 1, updated_at = now()
				WHERE user_id = $1 AND box = $2 AND version = $4 RETURNING version`, user, box, string(head), version).Scan(&next)
		}
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrStale
		case err != nil:
			return fmt.Errorf("swap head of %s: %w", box, err)
		}
		return notifyHead(ctx, tx, Head{User: user, Box: box, Head: head, Version: next})
	})
	return next, err
}

// DeleteHeads removes the head of user's box, or of every box of user when box is empty.
func (s *Store) DeleteHeads(ctx context.Context, user, box string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM box_heads WHERE user_id = $1 AND ($2 = '' OR box = $2)", user, box); err != nil {
			return fmt.Errorf("delete heads of %s: %w", user, err)
		}
		return notifyHead(ctx, tx, Head{User: user, Box: box})
	})
}

// DeleteHeadAt removes the head of user's box while it keeps version and reports whether it did.
func (s *Store) DeleteHeadAt(ctx context.Context, user, box string, version int64) (bool, error) {
	var removed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, "DELETE FROM box_heads WHERE user_id = $1 AND box = $2 AND version = $3", user, box, version)
		if err != nil {
			return fmt.Errorf("delete head of %s: %w", box, err)
		}
		if removed = cmd.RowsAffected() == 1; !removed {
			return nil
		}
		return notifyHead(ctx, tx, Head{User: user, Box: box})
	})
	return removed, err
}

// ParseHeadNotify returns the head a notify on ChannelHeads carries; Version 0 reports a removal, of every box of User when Box is empty.
func ParseHeadNotify(payload string) (Head, error) {
	var h Head
	if err := json.Unmarshal([]byte(payload), &h); err != nil {
		return Head{}, fmt.Errorf("decode a heads notify: %w", err)
	}
	return h, nil
}

func notifyHead(ctx context.Context, tx pgx.Tx, h Head) error {
	raw, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("encode the head of %s: %w", h.Box, err)
	}
	return notify(ctx, tx, ChannelHeads, string(raw))
}

func scanHead(row pgx.CollectableRow) (Head, error) {
	var h Head
	var head string
	err := row.Scan(&h.User, &h.Box, &head, &h.Version)
	h.Head = []byte(head)
	return h, err
}
