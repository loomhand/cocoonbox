package users

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The kinds of host-only secrets a push records.
const (
	PushModel   = "model"
	PushDoor    = "door"
	PushConnect = "connect"
	PushVault   = "vault"
)

// Push records that host-only secrets of one kind went to a machine on a node; it never holds a value.
type Push struct {
	Machine string
	Owner   string
	Kind    string
	Node    string
	At      time.Time
}

// RecordPushes records that secrets of kinds go to machine of owner on node.
func (s *Service) RecordPushes(ctx context.Context, machine, owner, node string, kinds []string) error {
	return s.store.exec(ctx, `INSERT INTO credential_pushes (machine_id, user_id, kind, node) SELECT $1, $2, unnest($3::text[]), $4
		ON CONFLICT (machine_id, kind, node) DO UPDATE SET user_id = EXCLUDED.user_id, pushed_at = now()`, machine, owner, kinds, node)
}

// PushesOn lists the pushes recorded for node.
func (s *Service) PushesOn(ctx context.Context, node string) ([]Push, error) {
	rows, err := s.store.pool.Query(ctx, "SELECT machine_id, user_id, kind, node, pushed_at FROM credential_pushes WHERE node = $1 ORDER BY machine_id, kind", node)
	if err != nil {
		return nil, fmt.Errorf("list pushes: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Push, error) {
		var p Push
		err := row.Scan(&p.Machine, &p.Owner, &p.Kind, &p.Node, &p.At)
		return p, err
	})
}

// DropPushesOn forgets the pushes recorded for node.
func (s *Service) DropPushesOn(ctx context.Context, node string) error {
	return s.store.exec(ctx, "DELETE FROM credential_pushes WHERE node = $1", node)
}
