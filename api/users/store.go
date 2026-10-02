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
		coalesce(token_sha256, ''), gw_ak_id, gw_key_sealed, tenant_token_sealed, coalesce(state_key_id, ''), state_key_sealed`
)

//go:embed migrations/*.sql
var migrations embed.FS

// store keeps users, their boxes and console sessions in PostgreSQL.
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
			&r.tokenSHA256, &r.gwAKID, &r.gwKey, &r.tenantToken, &r.stateKeyID, &r.stateKey)
		r.HasToken = r.tokenSHA256 != ""
		return r, err
	})
}

func (s *store) insert(ctx context.Context, r record, passwordHash string, first Box) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, name, admin, max_boxes, max_boxes_cap, password_hash, gw_ak_id, gw_key_sealed, tenant_token_sealed,
			state_key_id, state_key_sealed) VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, $9, $10, $11)`,
			r.ID, r.Name, r.Admin, r.MaxBoxes, r.MaxBoxesCap, passwordHash, r.gwAKID, r.gwKey, r.tenantToken, r.stateKeyID, r.stateKey); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO boxes (id, owner, name, created_at) VALUES ($1, $2, $3, $4)", first.ID, first.Owner, first.Name, first.CreatedAt)
		return err
	})
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		return ErrExists
	}
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

func (s *store) allBoxes(ctx context.Context) ([]Box, error) {
	rows, err := s.pool.Query(ctx, "SELECT id, owner, name, created_at FROM boxes ORDER BY created_at, id")
	if err != nil {
		return nil, fmt.Errorf("list boxes: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Box, error) {
		var b Box
		err := row.Scan(&b.ID, &b.Owner, &b.Name, &b.CreatedAt)
		return b, err
	})
}

// insertBox adds b while its owner has fewer boxes than max_boxes; the owner's row lock orders concurrent adds.
func (s *store) insertBox(ctx context.Context, b Box) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var maxBoxes, boxes int
		if err := tx.QueryRow(ctx, "SELECT max_boxes FROM users WHERE id = $1 FOR UPDATE", b.Owner).Scan(&maxBoxes); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM boxes WHERE owner = $1", b.Owner).Scan(&boxes); err != nil {
			return err
		}
		if boxes >= maxBoxes {
			return fmt.Errorf("%w: %d boxes is the most this user may have", ErrQuota, maxBoxes)
		}
		_, err := tx.Exec(ctx, "INSERT INTO boxes (id, owner, name, created_at) VALUES ($1, $2, $3, $4)", b.ID, b.Owner, b.Name, b.CreatedAt)
		return err
	})
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: a box is already named %q", ErrExists, b.Name)
	}
	if err != nil && !errors.Is(err, ErrQuota) && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("insert box: %w", err)
	}
	return err
}

func (s *store) renameBox(ctx context.Context, owner, id, name string) error {
	tag, err := s.pool.Exec(ctx, "UPDATE boxes SET name = $3 WHERE id = $1 AND owner = $2", id, owner, name)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: a box is already named %q", ErrExists, name)
	}
	if err != nil {
		return fmt.Errorf("rename box: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// removeBox deletes owner's box id and revokes its live share in one transaction.
func (s *store) removeBox(ctx context.Context, owner, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "DELETE FROM boxes WHERE id = $1 AND owner = $2", id, owner)
		if err != nil {
			return fmt.Errorf("delete box: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, "UPDATE shares SET revoked_at = now() WHERE box = $1 AND revoked_at IS NULL", id); err != nil {
			return fmt.Errorf("revoke the box's share: %w", err)
		}
		return nil
	})
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

func (s *store) setStateKey(ctx context.Context, r record) error {
	tag, err := s.pool.Exec(ctx, "UPDATE users SET state_key_id = $2, state_key_sealed = $3 WHERE id = $1 AND state_key_sealed IS NULL",
		r.ID, r.stateKeyID, r.stateKey)
	if err != nil {
		return fmt.Errorf("set state key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: user %s holds a state key or is gone", ErrExists, r.ID)
	}
	return nil
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

// insertShare stores rec as box's live share; an expired share left unrevoked gives way to it.
func (s *store) insertShare(ctx context.Context, rec *shareRecord) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE shares SET revoked_at = now() WHERE box = $1 AND revoked_at IS NULL AND expires_at <= now()", rec.box); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO shares (id, box, owner, secret_sha256, user_ids, write, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			rec.id, rec.box, rec.owner, rec.secretSHA256, rec.userIDs, rec.write, rec.expiresAt, rec.createdAt)
		return err
	})
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: the box is already shared", ErrExists)
	}
	if err != nil {
		return fmt.Errorf("insert share: %w", err)
	}
	return nil
}

func (s *store) liveShares(ctx context.Context) ([]*shareRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, box, owner, secret_sha256, user_ids, write, expires_at, created_at
		FROM shares WHERE revoked_at IS NULL AND expires_at > now()`)
	if err != nil {
		return nil, fmt.Errorf("list shares: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*shareRecord, error) {
		rec := &shareRecord{}
		err := row.Scan(&rec.id, &rec.box, &rec.owner, &rec.secretSHA256, &rec.userIDs, &rec.write, &rec.expiresAt, &rec.createdAt)
		return rec, err
	})
}

func (s *store) setShareWrite(ctx context.Context, id string, write bool) error {
	return s.execShare(ctx, "UPDATE shares SET write = $2 WHERE id = $1 AND revoked_at IS NULL", id, write)
}

func (s *store) setShareSecret(ctx context.Context, id, sha string) error {
	return s.execShare(ctx, "UPDATE shares SET secret_sha256 = $2 WHERE id = $1 AND revoked_at IS NULL", id, sha)
}

func (s *store) revokeShare(ctx context.Context, id string) error {
	return s.execShare(ctx, "UPDATE shares SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL", id)
}

// execShare runs an update of one live share; a share that is gone is ErrNotFound.
func (s *store) execShare(ctx context.Context, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("update share: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *store) exec(ctx context.Context, sql string, args ...any) error {
	if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("%s: %w", strings.Fields(sql)[0], err)
	}
	return nil
}
