package signup

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loomhand/cocoonbox/api/migrate"
)

const (
	takenNone taken = iota
	takenSubject
	takenUser

	settingsColumns = "enabled, invite_only, seats, allow_domains, allow_emails, daily_cap_micros, max_agent_boxes, hourly_accounts"
	// before orders the waiting entries: the earliest sort key, then the id.
	before = "(q.sort_key, q.id) < (e.sort_key, e.id)"
)

var (
	errNoInvite = errors.New("no usable invite")

	//go:embed migrations/*.sql
	migrations embed.FS
)

// taken says which link of an identity already exists: the IdP account's, or the user's own for that IdP.
type taken int

// querier reads and writes within a transaction or on the pool.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type store struct {
	pool *pgxpool.Pool
}

func openStore(ctx context.Context, dsn string) (*store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := migrate.Apply(ctx, pool, migrations, "signup_schema_migrations", migrationLock); err != nil {
		pool.Close()
		return nil, err
	}
	return &store{pool: pool}, nil
}

func (s *store) close() { s.pool.Close() }

func (s *store) settings(ctx context.Context, q querier) (Settings, error) {
	return scanSettings(q.QueryRow(ctx, "SELECT "+settingsColumns+" FROM signup_settings WHERE id = 1"))
}

func (s *store) lockSettings(ctx context.Context, tx pgx.Tx) (Settings, error) {
	return scanSettings(tx.QueryRow(ctx, "SELECT "+settingsColumns+" FROM signup_settings WHERE id = 1 FOR UPDATE"))
}

func (s *store) setSettings(ctx context.Context, st Settings) error {
	_, err := s.pool.Exec(ctx, `UPDATE signup_settings SET enabled = $1, invite_only = $2, seats = $3, allow_domains = $4, allow_emails = $5,
		daily_cap_micros = $6, max_agent_boxes = $7, hourly_accounts = $8 WHERE id = 1`,
		st.Enabled, st.InviteOnly, st.Seats, st.AllowDomains, st.AllowEmails, st.DailyCapMicros, st.MaxAgentBoxes, st.HourlyAccounts)
	if err != nil {
		return fmt.Errorf("set signup settings: %w", err)
	}
	return nil
}

func (s *store) seats(ctx context.Context) (Seats, error) {
	var out Seats
	err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users WHERE NOT admin AND NOT disabled), (SELECT seats FROM signup_settings WHERE id = 1),
		(SELECT count(*) FROM signup_queue WHERE status = 'waiting')`).Scan(&out.Used, &out.Cap, &out.Waiting)
	if err != nil {
		return Seats{}, fmt.Errorf("count seats: %w", err)
	}
	return out, nil
}

// room is how many people admission may take now: the free seats, within what the hourly brake leaves.
func (s *store) room(ctx context.Context, tx pgx.Tx, st Settings) (int, error) {
	var used, recent int
	err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM users WHERE NOT admin AND NOT disabled),
		(SELECT count(*) FROM signup_queue WHERE admitted_at > now() - interval '1 hour')`).Scan(&used, &recent)
	if err != nil {
		return 0, fmt.Errorf("count seats: %w", err)
	}
	return min(st.Seats-used, st.HourlyAccounts-recent), nil
}

func (s *store) waiting(ctx context.Context, tx pgx.Tx, limit int) ([]entry, error) {
	rows, err := tx.Query(ctx, `SELECT id, idp, subject, email, name_hint, secret_sha256 FROM signup_queue WHERE status = 'waiting'
		ORDER BY sort_key, id LIMIT $1 FOR UPDATE`, limit)
	if err != nil {
		return nil, fmt.Errorf("list waiting entries: %w", err)
	}
	return pgx.CollectRows(rows, scanEntry)
}

func (s *store) waitingEntry(ctx context.Context, tx pgx.Tx, id string) (entry, error) {
	rows, err := tx.Query(ctx, `SELECT id, idp, subject, email, name_hint, secret_sha256 FROM signup_queue WHERE id = $1 AND status = 'waiting' FOR UPDATE`, id)
	if err != nil {
		return entry{}, fmt.Errorf("read queue entry: %w", err)
	}
	e, err := pgx.CollectExactlyOneRow(rows, scanEntry)
	if errors.Is(err, pgx.ErrNoRows) {
		return entry{}, ErrNotFound
	}
	return e, err
}

func (s *store) markAdmitted(ctx context.Context, tx pgx.Tx, id, user string) error {
	_, err := tx.Exec(ctx, "UPDATE signup_queue SET status = 'admitted', admitted_at = now(), user_id = $2 WHERE id = $1", id, user)
	return err
}

func (s *store) admittedUser(ctx context.Context, id string) (string, error) {
	var user *string
	err := s.pool.QueryRow(ctx, "SELECT user_id FROM signup_queue WHERE id = $1 AND status = 'admitted'", id).Scan(&user)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read queue entry: %w", err)
	case user == nil:
		return "", nil
	}
	return *user, nil
}

// requeue gives the entry of an identity a new secret while it waits and returns its status, empty when it has none.
func (s *store) requeue(ctx context.Context, idp, subject, secretSHA256 string) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `UPDATE signup_queue SET secret_sha256 = CASE WHEN status = 'waiting' THEN $3 ELSE secret_sha256 END
		WHERE idp = $1 AND subject = $2 RETURNING status`, idp, subject, secretSHA256).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read queue entry: %w", err)
	}
	return status, nil
}

// enqueue adds e at the back, or with a usable invite at the front, spending one of the invite's uses.
func (s *store) enqueue(ctx context.Context, e entry, inviteSHA256 string, inviteOnly bool) (string, error) {
	id := "sq_" + randomHex(idBytes)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var invite *string
		if inviteSHA256 != "" {
			err := tx.QueryRow(ctx, `UPDATE signup_invites SET uses_left = uses_left - 1 WHERE code_sha256 = $1 AND uses_left > 0 AND expires_at > now()
				RETURNING id`, inviteSHA256).Scan(&invite)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("spend the invite: %w", err)
			}
		}
		if invite == nil && inviteOnly {
			return errNoInvite
		}
		sortKey := "now()"
		if invite != nil {
			sortKey = "coalesce((SELECT min(sort_key) FROM signup_queue WHERE status = 'waiting'), now()) - interval '1 microsecond'"
		}
		_, err := tx.Exec(ctx, `INSERT INTO signup_queue (id, idp, subject, email, name_hint, secret_sha256, sort_key, invite_id)
			VALUES ($1, $2, $3, $4, $5, $6, `+sortKey+`, $7)`, id, e.idp, e.subject, e.email, e.nameHint, e.secretSHA256, invite)
		if err != nil {
			return fmt.Errorf("add queue entry: %w", err)
		}
		return nil
	})
	return id, err
}

func (s *store) queue(ctx context.Context) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.id, e.idp, e.email, e.name_hint, e.status,
		CASE WHEN e.status = 'waiting' THEN (SELECT count(*) + 1 FROM signup_queue q WHERE q.status = 'waiting' AND `+before+`) ELSE 0 END,
		e.invite_id IS NOT NULL, e.created_at
		FROM signup_queue e ORDER BY e.status <> 'waiting', CASE WHEN e.status = 'waiting' THEN e.sort_key END, e.created_at DESC LIMIT $1`, maxListed)
	if err != nil {
		return nil, fmt.Errorf("list the queue: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := row.Scan(&e.ID, &e.IDP, &e.Email, &e.NameHint, &e.Status, &e.Position, &e.Invited, &e.CreatedAt)
		return e, err
	})
}

func (s *store) place(ctx context.Context, secretSHA256 string) (Place, error) {
	var p Place
	err := s.pool.QueryRow(ctx, `SELECT e.status, CASE WHEN e.status = 'waiting' THEN (SELECT count(*) + 1 FROM signup_queue q WHERE q.status = 'waiting' AND `+before+`) ELSE 0 END,
		(SELECT count(*) FROM signup_queue WHERE status = 'waiting'), e.email, e.created_at FROM signup_queue e WHERE e.secret_sha256 = $1`, secretSHA256).Scan(
		&p.Status, &p.Position, &p.Waiting, &p.Email, &p.Since)
	if errors.Is(err, pgx.ErrNoRows) {
		return Place{}, ErrNotFound
	}
	if err != nil {
		return Place{}, fmt.Errorf("read queue entry: %w", err)
	}
	return p, nil
}

// dropWaiting deletes the waiting entry of an identity that a signed-in user just linked, so admission never makes it a second account.
func (s *store) dropWaiting(ctx context.Context, idp, subject string) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM signup_queue WHERE idp = $1 AND subject = $2 AND status = 'waiting'", idp, subject); err != nil {
		return fmt.Errorf("drop queue entry: %w", err)
	}
	return nil
}

func (s *store) leave(ctx context.Context, secretSHA256 string) error {
	return s.one(ctx, "DELETE FROM signup_queue WHERE secret_sha256 = $1 AND status = 'waiting'", secretSHA256)
}

func (s *store) toFront(ctx context.Context, id string) error {
	return s.one(ctx, `UPDATE signup_queue SET sort_key = (SELECT min(sort_key) FROM signup_queue WHERE status = 'waiting') - interval '1 microsecond'
		WHERE id = $1 AND status = 'waiting'`, id)
}

func (s *store) reject(ctx context.Context, id string) error {
	return s.one(ctx, "UPDATE signup_queue SET status = 'rejected' WHERE id = $1 AND status = 'waiting'", id)
}

func (s *store) addInvite(ctx context.Context, inv Invite, codeSHA256 string) (Invite, error) {
	err := s.pool.QueryRow(ctx, `INSERT INTO signup_invites (id, code_sha256, uses_left, expires_at, created_by, token_id) VALUES ($1, $2, $3, $4, $5, nullif($6, ''))
		RETURNING created_at`, inv.ID, codeSHA256, inv.UsesLeft, inv.ExpiresAt, inv.CreatedBy, inv.TokenID).Scan(&inv.CreatedAt)
	if err != nil {
		return Invite{}, fmt.Errorf("add invite: %w", err)
	}
	return inv, nil
}

// invites lists the invites, only token's when it is not empty.
func (s *store) invites(ctx context.Context, token string) ([]Invite, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, uses_left, expires_at, created_by, coalesce(token_id, ''), created_at FROM signup_invites
		WHERE $1 = '' OR token_id = $1 ORDER BY created_at DESC LIMIT $2`, token, maxListed)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Invite, error) {
		var inv Invite
		err := row.Scan(&inv.ID, &inv.UsesLeft, &inv.ExpiresAt, &inv.CreatedBy, &inv.TokenID, &inv.CreatedAt)
		return inv, err
	})
}

func (s *store) addInviteToken(ctx context.Context, t InviteToken, sha string) (InviteToken, error) {
	err := s.pool.QueryRow(ctx, "INSERT INTO invite_tokens (id, sha256, label, created_by) VALUES ($1, $2, $3, $4) RETURNING created_at",
		t.ID, sha, t.Label, t.CreatedBy).Scan(&t.CreatedAt)
	if err != nil {
		return InviteToken{}, fmt.Errorf("add invite token: %w", err)
	}
	return t, nil
}

func (s *store) inviteTokens(ctx context.Context) ([]InviteToken, error) {
	rows, err := s.pool.Query(ctx, `SELECT t.id, t.label, t.created_by, t.created_at, t.used_at, (SELECT count(*) FROM signup_invites i WHERE i.token_id = t.id)
		FROM invite_tokens t ORDER BY t.created_at DESC LIMIT $1`, maxListed)
	if err != nil {
		return nil, fmt.Errorf("list invite tokens: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (InviteToken, error) {
		var t InviteToken
		err := row.Scan(&t.ID, &t.Label, &t.CreatedBy, &t.CreatedAt, &t.UsedAt, &t.Invites)
		return t, err
	})
}

func (s *store) inviteTokenBySHA256(ctx context.Context, sha string) (string, string, *time.Time, error) {
	var id, admin string
	var used *time.Time
	err := s.pool.QueryRow(ctx, "SELECT id, created_by, used_at FROM invite_tokens WHERE sha256 = $1", sha).Scan(&id, &admin, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil, ErrNotFound
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("read invite token: %w", err)
	}
	return id, admin, used, nil
}

func (s *store) touchInviteToken(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, "UPDATE invite_tokens SET used_at = now() WHERE id = $1", id); err != nil {
		return fmt.Errorf("record the use of invite token: %w", err)
	}
	return nil
}

func (s *store) removeInviteToken(ctx context.Context, id string) error {
	return s.one(ctx, "DELETE FROM invite_tokens WHERE id = $1", id)
}

func (s *store) removeInvite(ctx context.Context, id string) error {
	return s.one(ctx, "DELETE FROM signup_invites WHERE id = $1", id)
}

func (s *store) addFlow(ctx context.Context, nonceSHA256 string, f flow, expires time.Time) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM login_flows WHERE expires_at < now()"); err != nil {
			return fmt.Errorf("prune sign-ins: %w", err)
		}
		_, err := tx.Exec(ctx, `INSERT INTO login_flows (nonce_sha256, state, verifier, link_user, return_to, invite_sha256, expires_at)
			VALUES ($1, $2, $3, nullif($4, ''), $5, $6, $7)`, nonceSHA256, f.state, f.verifier, f.linkUser, f.returnTo, f.inviteSHA256, expires)
		if err != nil {
			return fmt.Errorf("add sign-in: %w", err)
		}
		return nil
	})
}

// takeFlow deletes the sign-in of the nonce and state and returns it while it lasts, once; a wrong state leaves it in place.
func (s *store) takeFlow(ctx context.Context, nonceSHA256, state string) (flow, error) {
	var f flow
	var expires time.Time
	err := s.pool.QueryRow(ctx, `DELETE FROM login_flows WHERE nonce_sha256 = $1 AND state = $2 RETURNING state, verifier, coalesce(link_user, ''), return_to,
		invite_sha256, expires_at`, nonceSHA256, state).Scan(&f.state, &f.verifier, &f.linkUser, &f.returnTo, &f.inviteSHA256, &expires)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && time.Now().After(expires) {
		return flow{}, errExpired
	}
	if err != nil {
		return flow{}, fmt.Errorf("take sign-in: %w", err)
	}
	return f, nil
}

func (s *store) identityUser(ctx context.Context, q querier, idp, subject string) (string, error) {
	var user string
	err := q.QueryRow(ctx, "SELECT user_id FROM identities WHERE idp = $1 AND subject = $2", idp, subject).Scan(&user)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read identity: %w", err)
	}
	return user, nil
}

// addIdentity links id to user unless the IdP account or the user's own link of idp exists, and says which did.
func (s *store) addIdentity(ctx context.Context, q querier, user, idp string, id identity) (taken, error) {
	tag, err := q.Exec(ctx, `INSERT INTO identities (idp, subject, user_id, email, email_verified) VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
		idp, id.subject, user, id.email, id.verified)
	if err != nil {
		return takenNone, fmt.Errorf("add identity: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return takenNone, nil
	}
	var subjectTaken bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM identities WHERE idp = $1 AND subject = $2)", idp, id.subject).Scan(&subjectTaken); err != nil {
		return takenNone, fmt.Errorf("read identity: %w", err)
	}
	if subjectTaken {
		return takenSubject, nil
	}
	return takenUser, nil
}

func (s *store) identities(ctx context.Context, user string) ([]Identity, error) {
	rows, err := s.pool.Query(ctx, "SELECT idp, email, created_at FROM identities WHERE user_id = $1 ORDER BY idp", user)
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Identity, error) {
		var id Identity
		err := row.Scan(&id.IDP, &id.Email, &id.CreatedAt)
		return id, err
	})
}

func (s *store) removeIdentity(ctx context.Context, user, idp string) error {
	return s.one(ctx, "DELETE FROM identities WHERE user_id = $1 AND idp = $2", user, idp)
}

// one runs a change that must touch a row, ErrNotFound when it touched none.
func (s *store) one(ctx context.Context, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("update signup: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanSettings(row pgx.Row) (Settings, error) {
	var st Settings
	err := row.Scan(&st.Enabled, &st.InviteOnly, &st.Seats, &st.AllowDomains, &st.AllowEmails, &st.DailyCapMicros, &st.MaxAgentBoxes, &st.HourlyAccounts)
	if err != nil {
		return Settings{}, fmt.Errorf("read signup settings: %w", err)
	}
	return st, nil
}

func scanEntry(row pgx.CollectableRow) (entry, error) {
	var e entry
	err := row.Scan(&e.id, &e.idp, &e.subject, &e.email, &e.nameHint, &e.secretSHA256)
	return e, err
}
