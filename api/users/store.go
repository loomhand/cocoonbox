package users

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	migrationLock = 0x63627573 // "cbus"

	uniqueViolation = "23505"

	recordColumns = `id, name, idp, admin, disabled, residential, max_boxes, max_boxes_cap, created_at,
		coalesce(token_sha256, ''), gw_ak_id, gw_key_sealed, tenant_token_sealed`
)

//go:embed migrations/*.sql
var migrations embed.FS

// store keeps users and console sessions in PostgreSQL.
type store struct {
	pool *pgxpool.Pool
}

func openStore(ctx context.Context, dsn string) (*store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) close() { s.pool.Close() }

// migrate applies the embedded migrations not yet recorded, one replica at a time.
func (s *store) migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock) }()
	if _, err = conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY)"); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(name, "migrations/"), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with its number", name)
		}
		var applied bool
		if err = conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&applied); err != nil {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		if applied {
			continue
		}
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
			return err
		}); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

func (s *store) all(ctx context.Context) ([]record, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+recordColumns+" FROM users ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (record, error) {
		var r record
		err := row.Scan(&r.ID, &r.Name, &r.IDP, &r.Admin, &r.Disabled, &r.Residential, &r.MaxBoxes, &r.MaxBoxesCap, &r.CreatedAt,
			&r.tokenSHA256, &r.gwAKID, &r.gwKey, &r.tenantToken)
		r.HasToken = r.tokenSHA256 != ""
		return r, err
	})
}

func (s *store) insert(ctx context.Context, r record, passwordHash string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO users (id, name, admin, max_boxes, max_boxes_cap, password_hash, gw_ak_id, gw_key_sealed, tenant_token_sealed)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, $9)`,
		r.ID, r.Name, r.Admin, r.MaxBoxes, r.MaxBoxesCap, passwordHash, r.gwAKID, r.gwKey, r.tenantToken)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		return ErrExists
	}
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

func (s *store) remove(ctx context.Context, id string) error {
	return s.exec(ctx, "DELETE FROM users WHERE id = $1", id)
}

func (s *store) setPassword(ctx context.Context, id, hash string) error {
	return s.exec(ctx, "UPDATE users SET password_hash = $2 WHERE id = $1", id, hash)
}

func (s *store) setToken(ctx context.Context, id, sha string) error {
	err := s.exec(ctx, "UPDATE users SET token_sha256 = $2 WHERE id = $1", id, sha)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		return ErrExists
	}
	return err
}

func (s *store) setSettings(ctx context.Context, r record) error {
	return s.exec(ctx, "UPDATE users SET disabled = $2, residential = $3, max_boxes = $4, max_boxes_cap = $5 WHERE id = $1",
		r.ID, r.Disabled, r.Residential, r.MaxBoxes, r.MaxBoxesCap)
}

// password returns the id and password hash of the enabled user name.
func (s *store) password(ctx context.Context, name string) (string, string, error) {
	var id, hash string
	err := s.pool.QueryRow(ctx, "SELECT id, coalesce(password_hash, '') FROM users WHERE name = $1 AND NOT disabled", name).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return id, hash, err
}

func (s *store) addSession(ctx context.Context, sha, user string, expires time.Time) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at < now()"); err != nil {
		return fmt.Errorf("purge sessions: %w", err)
	}
	return s.exec(ctx, "INSERT INTO sessions (id_sha256, user_id, expires_at) VALUES ($1, $2, $3)", sha, user, expires)
}

func (s *store) session(ctx context.Context, sha string) (string, error) {
	var user string
	err := s.pool.QueryRow(ctx, "SELECT user_id FROM sessions WHERE id_sha256 = $1 AND expires_at > now()", sha).Scan(&user)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return user, err
}

func (s *store) removeSession(ctx context.Context, sha string) error {
	return s.exec(ctx, "DELETE FROM sessions WHERE id_sha256 = $1", sha)
}

func (s *store) removeSessions(ctx context.Context, user string) error {
	return s.exec(ctx, "DELETE FROM sessions WHERE user_id = $1", user)
}

func (s *store) exec(ctx context.Context, sql string, args ...any) error {
	if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("%s: %w", strings.Fields(sql)[0], err)
	}
	return nil
}
