package users

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loomhand/cocoonbox/api/migrate"
)

const (
	migrationLock = 0x63627573 // "cbus"
	notifyChannel = "cocoonbox_users"
	endedChannel  = "cocoonbox_ended"

	uniqueViolation = "23505"

	recordColumns = `id, name, idp, admin, disabled, residential, max_boxes, max_boxes_cap, coalesce(max_running, 0), max_agent_boxes, created_at,
		coalesce(token_sha256, ''), gw_ak_id, gw_key_sealed, tenant_token_sealed, coalesce(state_key_id, ''), state_key_sealed`
)

//go:embed migrations/*.sql
var migrations embed.FS

// store keeps users, their boxes and console sessions in PostgreSQL.
type store struct {
	pool *pgxpool.Pool
}

func openStore(ctx context.Context, dsn string) (*store, error) {
	pool, err := migrate.Open(ctx, dsn, migrations, "schema_migrations", migrationLock)
	if err != nil {
		return nil, err
	}
	return &store{pool: pool}, nil
}

func (s *store) close() { s.pool.Close() }

func (s *store) notify(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, "SELECT pg_notify($1, '')", notifyChannel); err != nil {
		return fmt.Errorf("notify %s: %w", notifyChannel, err)
	}
	return nil
}

// listen takes a connection out of the pool and listens on notifyChannel and endedChannel with it; the caller closes it.
func (s *store) listen(ctx context.Context) (*pgx.Conn, error) {
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire a connection: %w", err)
	}
	conn := pooled.Hijack()
	for _, channel := range []string{notifyChannel, endedChannel} {
		if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("listen on %s: %w", channel, err)
		}
	}
	return conn, nil
}

func (s *store) notifyEnded(ctx context.Context, credential string) error {
	if _, err := s.pool.Exec(ctx, "SELECT pg_notify($1, $2)", endedChannel, credential); err != nil {
		return fmt.Errorf("notify %s: %w", endedChannel, err)
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
		err := row.Scan(&r.ID, &r.Name, &r.IDP, &r.Admin, &r.Disabled, &r.Residential, &r.MaxBoxes, &r.MaxBoxesCap, &r.MaxRunning, &r.MaxAgentBoxes, &r.CreatedAt,
			&r.tokenSHA256, &r.gwAKID, &r.gwKey, &r.tenantToken, &r.stateKeyID, &r.stateKey)
		r.HasToken = r.tokenSHA256 != ""
		return r, err
	})
}

func (s *store) insert(ctx context.Context, r record, passwordHash string, first Box) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, name, idp, admin, max_boxes, max_boxes_cap, max_agent_boxes, password_hash, gw_ak_id, gw_key_sealed,
			tenant_token_sealed, state_key_id, state_key_sealed) VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), $9, $10, $11, $12, $13)`,
			r.ID, r.Name, r.IDP, r.Admin, r.MaxBoxes, r.MaxBoxesCap, r.MaxAgentBoxes, passwordHash, r.gwAKID, r.gwKey, r.tenantToken, r.stateKeyID, r.stateKey); err != nil {
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
	rows, err := s.pool.Query(ctx, "SELECT id, owner, name, agent, created_at FROM boxes ORDER BY created_at, id")
	if err != nil {
		return nil, fmt.Errorf("list boxes: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Box, error) {
		var b Box
		err := row.Scan(&b.ID, &b.Owner, &b.Name, &b.Agent, &b.CreatedAt)
		return b, err
	})
}

// insertBox adds b while the owner has fewer boxes of b's kind than max_boxes (its own) or max_agent_boxes (its agents'); the owner's
// row lock orders concurrent adds.
func (s *store) insertBox(ctx context.Context, b Box) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var maxBoxes, boxes int
		var maxAgentBoxes *int
		err := tx.QueryRow(ctx, "SELECT max_boxes, max_agent_boxes FROM users WHERE id = $1 FOR UPDATE", b.Owner).Scan(&maxBoxes, &maxAgentBoxes)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM boxes WHERE owner = $1 AND agent = $2", b.Owner, b.Agent).Scan(&boxes); err != nil {
			return err
		}
		switch {
		case !b.Agent && boxes >= maxBoxes:
			return fmt.Errorf("%w: %d boxes is the most this user may have", ErrQuota, maxBoxes)
		case b.Agent && maxAgentBoxes != nil && boxes >= *maxAgentBoxes:
			return fmt.Errorf("%w: %d agent boxes is the most this user's agents may hold together", ErrQuota, *maxAgentBoxes)
		}
		_, err = tx.Exec(ctx, "INSERT INTO boxes (id, owner, name, agent, created_at) VALUES ($1, $2, $3, $4, $5)", b.ID, b.Owner, b.Name, b.Agent, b.CreatedAt)
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
		if _, err := tx.Exec(ctx, "UPDATE shares SET revoked_at = now(), secret_sealed = NULL WHERE box = $1 AND revoked_at IS NULL", id); err != nil {
			return fmt.Errorf("revoke the box's share: %w", err)
		}
		return nil
	})
}

func (s *store) remove(ctx context.Context, id string) error {
	return s.exec(ctx, "DELETE FROM users WHERE id = $1", id)
}

func (s *store) update(ctx context.Context, r record) error {
	return s.exec(ctx, `UPDATE users SET disabled = $2, residential = $3, max_boxes = $4, max_boxes_cap = $5, max_running = nullif($6, 0),
		max_agent_boxes = $7, admin = $8 WHERE id = $1`, r.ID, r.Disabled, r.Residential, r.MaxBoxes, r.MaxBoxesCap, r.MaxRunning, r.MaxAgentBoxes, r.Admin)
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

func (s *store) clearToken(ctx context.Context, id string) error {
	return s.exec(ctx, "UPDATE users SET token_sha256 = NULL WHERE id = $1", id)
}

func (s *store) tokenServes(ctx context.Context, sha string) (bool, error) {
	var serves bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE token_sha256 = $1 AND NOT disabled)
		OR EXISTS (SELECT 1 FROM user_tokens t JOIN users u ON u.id = t.user_id WHERE t.sha256 = $1 AND NOT u.disabled)`, sha).Scan(&serves)
	return serves, err
}

// allDevices returns every device token by user id.
func (s *store) allDevices(ctx context.Context) (map[string][]deviceToken, error) {
	rows, err := s.pool.Query(ctx, "SELECT user_id, id, sha256 FROM user_tokens ORDER BY created_at, id")
	if err != nil {
		return nil, fmt.Errorf("list device tokens: %w", err)
	}
	byUser := map[string][]deviceToken{}
	var user string
	var d deviceToken
	_, err = pgx.ForEachRow(rows, []any{&user, &d.id, &d.sha256}, func() error {
		byUser[user] = append(byUser[user], d)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list device tokens: %w", err)
	}
	return byUser, nil
}

func (s *store) insertDevice(ctx context.Context, user string, d deviceToken, client, label string) error {
	return s.exec(ctx, "INSERT INTO user_tokens (id, user_id, sha256, client, label) VALUES ($1, $2, $3, $4, $5)", d.id, user, d.sha256, client, label)
}

func (s *store) devices(ctx context.Context, user string) ([]Device, error) {
	rows, err := s.pool.Query(ctx, "SELECT id, client, label, created_at, used_at FROM user_tokens WHERE user_id = $1 ORDER BY created_at DESC, id", user)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Device, error) {
		var d Device
		err := row.Scan(&d.ID, &d.Client, &d.Label, &d.CreatedAt, &d.UsedAt)
		return d, err
	})
}

// removeDevice deletes user's device id and returns its token's sha256.
func (s *store) removeDevice(ctx context.Context, user, id string) (string, error) {
	var sum string
	err := s.pool.QueryRow(ctx, "DELETE FROM user_tokens WHERE id = $1 AND user_id = $2 RETURNING sha256", id, user).Scan(&sum)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("delete device: %w", err)
	}
	return sum, nil
}

// removeDeviceBySHA256 deletes the device token that hashes to sum and returns its user and device id, both empty when none does.
func (s *store) removeDeviceBySHA256(ctx context.Context, sum string) (string, string, error) {
	var user, id string
	err := s.pool.QueryRow(ctx, "DELETE FROM user_tokens WHERE sha256 = $1 RETURNING user_id, id", sum).Scan(&user, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("delete device: %w", err)
	}
	return user, id, nil
}

func (s *store) signOut(ctx context.Context, user string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, sql := range []string{"DELETE FROM sessions WHERE user_id = $1", "UPDATE users SET token_sha256 = NULL WHERE id = $1", "DELETE FROM user_tokens WHERE user_id = $1"} {
			if _, err := tx.Exec(ctx, sql, user); err != nil {
				return fmt.Errorf("sign out: %w", err)
			}
		}
		return nil
	})
}

func (s *store) touchDevice(ctx context.Context, id string) error {
	return s.exec(ctx, "UPDATE user_tokens SET used_at = now() WHERE id = $1", id)
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

// insertShare stores rec as box's live share with its sealed secret; an expired share left unrevoked gives way to it.
func (s *store) insertShare(ctx context.Context, rec *shareRecord, sealed []byte) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE shares SET revoked_at = now(), secret_sealed = NULL WHERE box = $1 AND revoked_at IS NULL AND expires_at <= now()",
			rec.box); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO shares (id, box, owner, secret_sha256, secret_sealed, user_ids, write, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			rec.id, rec.box, rec.owner, rec.secretSHA256, sealed, rec.userIDs, rec.write, rec.expiresAt, rec.createdAt)
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

// forgetEndedSecrets drops the secrets of the shares that ended while no api ran.
func (s *store) forgetEndedSecrets(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, "UPDATE shares SET secret_sealed = NULL WHERE secret_sealed IS NOT NULL AND (revoked_at IS NOT NULL OR expires_at <= now())"); err != nil {
		return fmt.Errorf("clear ended share secrets: %w", err)
	}
	return nil
}

func (s *store) liveShares(ctx context.Context) ([]*shareRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, box, owner, secret_sha256, secret_sealed, user_ids, write, expires_at, created_at
		FROM shares WHERE revoked_at IS NULL AND expires_at > now()`)
	if err != nil {
		return nil, fmt.Errorf("list shares: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*shareRecord, error) {
		rec := &shareRecord{}
		err := row.Scan(&rec.id, &rec.box, &rec.owner, &rec.secretSHA256, &rec.secretSealed, &rec.userIDs, &rec.write, &rec.expiresAt, &rec.createdAt)
		return rec, err
	})
}

func (s *store) setShareWrite(ctx context.Context, id string, write bool) error {
	return s.execShare(ctx, "UPDATE shares SET write = $2 WHERE id = $1 AND revoked_at IS NULL", id, write)
}

func (s *store) setShareSecret(ctx context.Context, id, sha string, sealed []byte) error {
	return s.execShare(ctx, "UPDATE shares SET secret_sha256 = $2, secret_sealed = $3 WHERE id = $1 AND revoked_at IS NULL", id, sha, sealed)
}

func (s *store) revokeShare(ctx context.Context, id string) error {
	return s.execShare(ctx, "UPDATE shares SET revoked_at = now(), secret_sealed = NULL WHERE id = $1 AND revoked_at IS NULL", id)
}

func (s *store) clearShareSecret(ctx context.Context, id string) error {
	return s.exec(ctx, "UPDATE shares SET secret_sealed = NULL WHERE id = $1", id)
}

// putWakeSchedule upserts w while its owner has fewer than maxSchedules schedules on other boxes; the owner's row lock orders concurrent puts.
func (s *store) putWakeSchedule(ctx context.Context, w WakeSchedule, maxSchedules int) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT id FROM users WHERE id = $1 FOR UPDATE", w.Owner).Scan(new(string)); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		var owned bool
		var others int
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM boxes WHERE id = $1 AND owner = $2),
			(SELECT count(*) FROM wake_schedules WHERE owner = $2 AND box <> $1)`, w.Box, w.Owner).Scan(&owned, &others); err != nil {
			return fmt.Errorf("count wake schedules: %w", err)
		}
		if !owned {
			return ErrNotFound
		}
		if others >= maxSchedules {
			return fmt.Errorf("%w: %d wake schedules is the most this user may have", ErrQuota, maxSchedules)
		}
		_, err := tx.Exec(ctx, `INSERT INTO wake_schedules (box, owner, cron, tz, awake_for_s, next_at) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (box) DO UPDATE SET cron = excluded.cron, tz = excluded.tz, awake_for_s = excluded.awake_for_s, next_at = excluded.next_at`,
			w.Box, w.Owner, w.Cron, w.TZ, int(w.AwakeFor/time.Second), w.NextAt)
		if err != nil {
			return fmt.Errorf("put wake schedule: %w", err)
		}
		return nil
	})
}

func (s *store) wakeSchedules(ctx context.Context, owner string) ([]WakeSchedule, error) {
	rows, err := s.pool.Query(ctx, `SELECT w.box, w.owner, w.cron, w.tz, w.awake_for_s, w.next_at, w.last_wake_at, w.sleep_at
		FROM wake_schedules w JOIN users u ON u.id = w.owner WHERE w.owner = $1 OR ($1 = '' AND NOT u.disabled) ORDER BY w.created_at, w.box`, owner)
	if err != nil {
		return nil, fmt.Errorf("list wake schedules: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (WakeSchedule, error) {
		var w WakeSchedule
		var awakeFor int
		var lastWake, sleep *time.Time
		err := row.Scan(&w.Box, &w.Owner, &w.Cron, &w.TZ, &awakeFor, &w.NextAt, &lastWake, &sleep)
		w.AwakeFor = time.Duration(awakeFor) * time.Second
		if lastWake != nil {
			w.LastWakeAt = *lastWake
		}
		if sleep != nil {
			w.SleepAt = *sleep
		}
		return w, err
	})
}

func (s *store) removeWakeSchedule(ctx context.Context, owner, box string) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM wake_schedules WHERE box = $1 AND owner = $2", box, owner)
	if err != nil {
		return fmt.Errorf("delete wake schedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// setWakeAlarm updates w's alarm state, ErrNotFound when its schedule changed or went since w was read.
func (s *store) setWakeAlarm(ctx context.Context, w WakeSchedule) error {
	tag, err := s.pool.Exec(ctx, `UPDATE wake_schedules SET next_at = $2, last_wake_at = $3, sleep_at = $4
		WHERE box = $1 AND cron = $5 AND tz = $6 AND awake_for_s = $7`,
		w.Box, w.NextAt, nullTime(w.LastWakeAt), nullTime(w.SleepAt), w.Cron, w.TZ, int(w.AwakeFor/time.Second))
	if err != nil {
		return fmt.Errorf("set wake alarm: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return new(t)
}
