package vault

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loomhand/cocoonbox/api/migrate"
)

const (
	migrationLock   = 0x63627674 // "cbvt"
	uniqueViolation = "23505"

	rowColumns = "id, owner, name, hosts, header, template, query, placeholder, value_sealed, value_hint, value_len, version, created_at, updated_at"
)

//go:embed migrations/*.sql
var migrations embed.FS

type store struct {
	pool *pgxpool.Pool
}

func openStore(ctx context.Context, dsn string) (*store, error) {
	pool, err := migrate.Open(ctx, dsn, migrations, "vault_schema_migrations", migrationLock)
	if err != nil {
		return nil, err
	}
	return &store{pool: pool}, nil
}

func (s *store) close() { s.pool.Close() }

func (s *store) secrets(ctx context.Context, owner string) ([]*secret, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+rowColumns+" FROM vault_secrets WHERE owner = $1 ORDER BY name", owner)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	return pgx.CollectRows(rows, scanSecret)
}

// versions returns every owner's key ids with their versions.
func (s *store) versions(ctx context.Context) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, "SELECT owner, id || ':' || version FROM vault_secrets")
	if err != nil {
		return nil, fmt.Errorf("list key versions: %w", err)
	}
	out := map[string][]string{}
	var owner, version string
	_, err = pgx.ForEachRow(rows, []any{&owner, &version}, func() error {
		out[owner] = append(out[owner], version)
		return nil
	})
	return out, err
}

// insert adds sec once check passes on the owner's other keys; the owner's row lock orders concurrent adds and rotates.
func (s *store) insert(ctx context.Context, sec *secret, check func([]*secret) error) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		others, err := owned(ctx, tx, sec.owner)
		if err != nil {
			return err
		}
		if err := check(others); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO vault_secrets (id, owner, name, hosts, header, template, query, placeholder, value_sealed, value_hint,
			value_len) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING created_at, updated_at`,
			sec.id, sec.owner, sec.name, sec.hosts, sec.header, sec.template, sec.query, sec.placeholder, sec.sealed, sec.hint,
			sec.valueLen).Scan(&sec.createdAt, &sec.updatedAt)
	})
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: a key is already named %s", errExists, sec.name)
	}
	return err
}

// rotate replaces the value of owner's key name with what fn seals for the row, once check passes on the owner's keys, and bumps its version.
func (s *store) rotate(ctx context.Context, owner, name string, fn func(*secret) error, check func([]*secret) error) (*secret, error) {
	var sec *secret
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		list, err := owned(ctx, tx, owner)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(list, func(k *secret) bool { return k.name == name })
		if i < 0 {
			return errNoKey
		}
		sec = list[i]
		if err := fn(sec); err != nil {
			return err
		}
		if err := check(list); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE vault_secrets SET value_sealed = $2, value_hint = $3, value_len = $4,
			version = version + 1, updated_at = now() WHERE id = $1 RETURNING version, updated_at`,
			sec.id, sec.sealed, sec.hint, sec.valueLen).Scan(&sec.version, &sec.updatedAt)
	})
	return sec, err
}

func (s *store) remove(ctx context.Context, owner, name string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, "DELETE FROM vault_secrets WHERE owner = $1 AND name = $2 RETURNING id", owner, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNoKey
	}
	if err != nil {
		return "", fmt.Errorf("delete key: %w", err)
	}
	return id, nil
}

// audit records op and drops the records past the retention.
func (s *store) audit(ctx context.Context, owner, id, name, op, actor string) error {
	if _, err := s.pool.Exec(ctx, "INSERT INTO vault_audit (owner, secret_id, name, op, actor) VALUES ($1, $2, $3, $4, $5)", owner, id, name, op, actor); err != nil {
		return fmt.Errorf("write audit: %w", err)
	}
	if _, err := s.pool.Exec(ctx, "DELETE FROM vault_audit WHERE at < $1", time.Now().Add(-auditRetention)); err != nil {
		return fmt.Errorf("purge audit: %w", err)
	}
	return nil
}

func owned(ctx context.Context, tx pgx.Tx, owner string) ([]*secret, error) {
	if err := tx.QueryRow(ctx, "SELECT id FROM users WHERE id = $1 FOR UPDATE", owner).Scan(new(string)); err != nil {
		return nil, fmt.Errorf("lock user: %w", err)
	}
	rows, err := tx.Query(ctx, "SELECT "+rowColumns+" FROM vault_secrets WHERE owner = $1", owner)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	return pgx.CollectRows(rows, scanSecret)
}

func scanSecret(row pgx.CollectableRow) (*secret, error) {
	sec := &secret{}
	err := row.Scan(&sec.id, &sec.owner, &sec.name, &sec.hosts, &sec.header, &sec.template, &sec.query, &sec.placeholder, &sec.sealed,
		&sec.hint, &sec.valueLen, &sec.version, &sec.createdAt, &sec.updatedAt)
	return sec, err
}
