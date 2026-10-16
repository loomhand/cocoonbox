package oauth

import (
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loomhand/cocoonbox/api/migrate"
)

const (
	migrationLock = 0x63626f61 // "cboa"

	grantColumns = `g.id, g.user_id, g.client_id, g.client_name, g.client_host, g.max_boxes, g.max_running, g.created_at, g.expires_at,
		g.used_at, g.seen, ARRAY(SELECT b.box_id FROM oauth_boxes b WHERE b.grant_id = g.id ORDER BY b.box_id)`
)

//go:embed migrations/*.sql
var migrations embed.FS

// code is an authorization code's consent, kept until the client redeems it once; label names a first-party client's device.
type code struct {
	user         string
	client       client
	redirectURI  string
	challenge    string
	caps         Caps
	label        string
	grantExpires time.Time
	expiresAt    time.Time
	codeSHA256   string
}

// querier reads within a transaction or from the pool.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// issued holds the sha256 of a new access token and of its refresh token.
type issued struct {
	access  string
	refresh string
}

type store struct {
	pool *pgxpool.Pool
}

func openStore(ctx context.Context, dsn string) (*store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := migrate.Apply(ctx, pool, migrations, "oauth_schema_migrations", migrationLock); err != nil {
		pool.Close()
		return nil, err
	}
	return &store{pool: pool}, nil
}

func (s *store) close() { s.pool.Close() }

// key returns the secret that signs registered client ids, made once for every replica.
func (s *store) key(ctx context.Context) ([]byte, error) {
	secret := make([]byte, secretBytes)
	_, _ = rand.Read(secret)
	if _, err := s.pool.Exec(ctx, "INSERT INTO oauth_key (id, secret) VALUES (1, $1) ON CONFLICT DO NOTHING", secret); err != nil {
		return nil, fmt.Errorf("make client key: %w", err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT secret FROM oauth_key WHERE id = 1").Scan(&secret); err != nil {
		return nil, fmt.Errorf("read client key: %w", err)
	}
	return secret, nil
}

func (s *store) addCode(ctx context.Context, c code) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM oauth_codes WHERE expires_at < now()"); err != nil {
			return fmt.Errorf("prune codes: %w", err)
		}
		_, err := tx.Exec(ctx, `INSERT INTO oauth_codes (code_sha256, user_id, client_id, client_name, client_host, redirect_uri,
			code_challenge, max_boxes, max_running, grant_expires_at, expires_at, account, label)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			c.codeSHA256, c.user, c.client.id, c.client.name, c.client.host, c.redirectURI, c.challenge, c.caps.MaxBoxes, c.caps.MaxRunning,
			c.grantExpires, c.expiresAt, c.client.firstParty, c.label)
		if err != nil {
			return fmt.Errorf("add code: %w", err)
		}
		return nil
	})
}

// redeem takes the code once and, when check accepts it, issues tokens on a new grant, or on the client's live grant of the
// user, which takes the new consent's caps and expiry, so a client that signs in again keeps its boxes. A first-party
// client's code gets no grant: the caller issues its device token.
func (s *store) redeem(ctx context.Context, codeSHA256, grantID string, tokens issued, check func(code) error) (code, error) {
	stored := code{codeSHA256: codeSHA256}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `DELETE FROM oauth_codes WHERE code_sha256 = $1 RETURNING user_id, client_id, client_name, client_host,
			redirect_uri, code_challenge, max_boxes, max_running, grant_expires_at, expires_at, account, label`, codeSHA256).Scan(
			&stored.user, &stored.client.id, &stored.client.name, &stored.client.host, &stored.redirectURI, &stored.challenge,
			&stored.caps.MaxBoxes, &stored.caps.MaxRunning, &stored.grantExpires, &stored.expiresAt, &stored.client.firstParty, &stored.label)
		if errors.Is(err, pgx.ErrNoRows) {
			return errInvalidGrant
		}
		if err != nil {
			return fmt.Errorf("take code: %w", err)
		}
		if err = check(stored); err != nil || stored.client.firstParty {
			return err
		}
		if _, err = tx.Exec(ctx, "SELECT id FROM users WHERE id = $1 FOR UPDATE", stored.user); err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		existing, err := liveGrant(ctx, tx, stored.user, stored.client.id)
		if err != nil {
			return err
		}
		if existing != "" {
			grantID = existing
			_, err = tx.Exec(ctx, `UPDATE oauth_grants SET client_name = $2, client_host = $3, max_boxes = $4, max_running = $5, expires_at = $6
				WHERE id = $1`, grantID, stored.client.name, stored.client.host, stored.caps.MaxBoxes, stored.caps.MaxRunning, stored.grantExpires)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO oauth_grants (id, user_id, client_id, client_name, client_host, max_boxes, max_running, expires_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, grantID, stored.user, stored.client.id, stored.client.name, stored.client.host,
				stored.caps.MaxBoxes, stored.caps.MaxRunning, stored.grantExpires)
		}
		if err != nil {
			return fmt.Errorf("add grant: %w", err)
		}
		return insertTokens(ctx, tx, grantID, tokens, stored.grantExpires)
	})
	return stored, err
}

// refresh spends the refresh token and issues tokens in its place; a spent one revokes its grant, as a replay shows a leak.
func (s *store) refresh(ctx context.Context, refreshSHA256, clientID string, tokens issued) error {
	var replay bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			grantID, owner string
			spent          bool
			expiresAt      time.Time
		)
		err := tx.QueryRow(ctx, `SELECT g.id, g.client_id, t.spent, g.expires_at FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id
			WHERE t.sha256 = $1 AND t.refresh FOR UPDATE OF g, t`, refreshSHA256).Scan(&grantID, &owner, &spent, &expiresAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return errInvalidGrant
		case err != nil:
			return fmt.Errorf("read refresh token: %w", err)
		case owner != clientID:
			return errInvalidGrant
		case spent:
			replay = true
			_, err = tx.Exec(ctx, "DELETE FROM oauth_grants WHERE id = $1", grantID)
			return err
		case !time.Now().Before(expiresAt):
			return errInvalidGrant
		}
		if _, err := tx.Exec(ctx, "DELETE FROM oauth_tokens WHERE grant_id = $1 AND (spent OR (NOT refresh AND expires_at < now()))", grantID); err != nil {
			return fmt.Errorf("prune tokens: %w", err)
		}
		if _, err := tx.Exec(ctx, "UPDATE oauth_tokens SET spent = true WHERE sha256 = $1", refreshSHA256); err != nil {
			return fmt.Errorf("spend refresh token: %w", err)
		}
		return insertTokens(ctx, tx, grantID, tokens, expiresAt)
	})
	if err == nil && replay {
		return errInvalidGrant
	}
	return err
}

func (s *store) liveGrant(ctx context.Context, user, client string) (string, error) {
	return liveGrant(ctx, s.pool, user, client)
}

func (s *store) byAccess(ctx context.Context, sum string) (Grant, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+grantColumns+` FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id
		WHERE t.sha256 = $1 AND NOT t.refresh AND t.expires_at > now() AND g.expires_at > now()`, sum)
	if err != nil {
		return Grant{}, fmt.Errorf("read access token: %w", err)
	}
	g, err := pgx.CollectExactlyOneRow(rows, scanGrant)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrInvalidToken
	}
	return g, err
}

func (s *store) touch(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, "UPDATE oauth_grants SET used_at = now() WHERE id = $1", id)
	return err
}

func (s *store) grants(ctx context.Context, user string) ([]Grant, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+grantColumns+" FROM oauth_grants g WHERE g.user_id = $1 ORDER BY g.created_at DESC", user)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	return pgx.CollectRows(rows, scanGrant)
}

func (s *store) revoke(ctx context.Context, user, id string) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM oauth_grants WHERE id = $1 AND user_id = $2", id, user)
	if err != nil {
		return fmt.Errorf("revoke grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *store) revokeByToken(ctx context.Context, sum, clientID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM oauth_grants g USING oauth_tokens t WHERE t.grant_id = g.id AND t.sha256 = $1 AND g.client_id = $2`, sum, clientID)
	return err
}

func (s *store) markSeen(ctx context.Context, user string) error {
	_, err := s.pool.Exec(ctx, "UPDATE oauth_grants SET seen = true WHERE user_id = $1 AND NOT seen", user)
	return err
}

func (s *store) grantBoxes(ctx context.Context, id string) ([]string, error) {
	rows, err := s.pool.Query(ctx, "SELECT box_id FROM oauth_boxes WHERE grant_id = $1 ORDER BY box_id", id)
	if err != nil {
		return nil, fmt.Errorf("list grant boxes: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *store) addBox(ctx context.Context, grant, box string) error {
	_, err := s.pool.Exec(ctx, "INSERT INTO oauth_boxes (box_id, grant_id) VALUES ($1, $2)", box, grant)
	return err
}

func liveGrant(ctx context.Context, q querier, user, client string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id FROM oauth_grants WHERE user_id = $1 AND client_id = $2 AND expires_at > now()
		ORDER BY created_at DESC LIMIT 1`, user, client).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read live grant: %w", err)
	}
	return id, nil
}

func insertTokens(ctx context.Context, tx pgx.Tx, grantID string, tokens issued, grantExpires time.Time) error {
	accessExpires := time.Now().Add(accessTTL)
	if grantExpires.Before(accessExpires) {
		accessExpires = grantExpires
	}
	_, err := tx.Exec(ctx, "INSERT INTO oauth_tokens (sha256, grant_id, refresh, expires_at) VALUES ($1, $2, false, $3), ($4, $2, true, $5)",
		tokens.access, grantID, accessExpires, tokens.refresh, grantExpires)
	if err != nil {
		return fmt.Errorf("add tokens: %w", err)
	}
	return nil
}

func scanGrant(row pgx.CollectableRow) (Grant, error) {
	var g Grant
	err := row.Scan(&g.ID, &g.User, &g.ClientID, &g.ClientName, &g.ClientHost, &g.MaxBoxes, &g.MaxRunning, &g.CreatedAt, &g.ExpiresAt,
		&g.UsedAt, &g.Seen, &g.Boxes)
	return g, err
}
