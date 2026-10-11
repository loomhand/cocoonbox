package connect

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CMGS/cocoonbox/api/migrate"
)

const (
	migrationLock = 0x63626374 // "cbct"

	grantColumns = "id, user_id, connector, kind, mcp_url, account_label, scopes, status, policy, sealed, token_expires_at, created_at"
)

//go:embed migrations/*.sql
var migrations embed.FS

type grant struct {
	id        string
	user      string
	connector string
	kind      string
	mcpURL    string
	account   string
	scopes    []string
	status    string
	policy    map[string]string
	sealed    []byte
	expiresAt *time.Time
	createdAt time.Time
}

type flow struct {
	id            string
	user          string
	connector     string
	kind          string
	mcpURL        string
	stateSHA256   string
	nonceSHA256   string
	sessionSHA256 string
	sealed        []byte
	expiresAt     time.Time
}

type approval struct {
	id         string
	user       string
	grantID    string
	connector  string
	tool       string
	argsSHA256 string
	summary    string
	expiresAt  time.Time
}

type record struct {
	at         time.Time
	user       string
	connector  string
	tool       string
	door       string
	caller     string
	decision   string
	outcome    string
	status     int
	latency    time.Duration
	reqBytes   int64
	respBytes  int64
	argsSHA256 string
}

type store struct {
	pool *pgxpool.Pool
}

func openStore(ctx context.Context, dsn string) (*store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := migrate.Apply(ctx, pool, migrations, "connect_schema_migrations", migrationLock); err != nil {
		pool.Close()
		return nil, err
	}
	return &store{pool: pool}, nil
}

func (s *store) close() { s.pool.Close() }

func (s *store) grants(ctx context.Context, user string) ([]*grant, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+grantColumns+" FROM connect_grants WHERE user_id = $1 ORDER BY created_at, connector", user)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	return pgx.CollectRows(rows, scanGrant)
}

// activeUsers lists the users with an active grant of kind.
func (s *store) activeUsers(ctx context.Context, kind string) ([]string, error) {
	rows, err := s.pool.Query(ctx, "SELECT user_id FROM connect_grants WHERE kind = $1 AND status = $2 ORDER BY user_id", kind, statusActive)
	if err != nil {
		return nil, fmt.Errorf("list the users of %s: %w", kind, err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *store) grant(ctx context.Context, user, connector string) (*grant, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+grantColumns+" FROM connect_grants WHERE user_id = $1 AND connector = $2", user, connector)
	if err != nil {
		return nil, fmt.Errorf("read grant: %w", err)
	}
	g, err := pgx.CollectExactlyOneRow(rows, scanGrant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoGrant
	}
	return g, err
}

// putGrant replaces the user's grant of the connector, keeps its policy, and seals for the row id it ends up with.
func (s *store) putGrant(ctx context.Context, g *grant, seal func(id string) ([]byte, error)) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, "SELECT id FROM connect_grants WHERE user_id = $1 AND connector = $2 FOR UPDATE", g.user, g.connector).Scan(&g.id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read grant: %w", err)
		}
		if g.id == "" {
			g.id = "cg_" + randomHex(idBytes)
		}
		if g.sealed, err = seal(g.id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO connect_grants (id, user_id, connector, kind, mcp_url, account_label, scopes, status, sealed, token_expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, coalesce($7, '{}'::text[]), $8, $9, $10)
			ON CONFLICT (id) DO UPDATE SET kind = $4, mcp_url = $5, account_label = $6, scopes = coalesce($7, '{}'::text[]), status = $8, sealed = $9,
				token_expires_at = $10, updated_at = now()`,
			g.id, g.user, g.connector, g.kind, g.mcpURL, g.account, g.scopes, statusActive, g.sealed, g.expiresAt)
		if err != nil {
			return fmt.Errorf("store grant: %w", err)
		}
		return nil
	})
}

// refreshGrant stores the grant fn returns under the row lock and returns fn's error after the commit.
func (s *store) refreshGrant(ctx context.Context, id string, fn func(*grant) (*grant, error)) error {
	var fnErr error
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT "+grantColumns+" FROM connect_grants WHERE id = $1 FOR UPDATE", id)
		if err != nil {
			return fmt.Errorf("lock grant: %w", err)
		}
		g, err := pgx.CollectExactlyOneRow(rows, scanGrant)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoGrant
		}
		if err != nil {
			return err
		}
		var next *grant
		if next, fnErr = fn(g); next == nil {
			return nil
		}
		if _, err := tx.Exec(ctx, "UPDATE connect_grants SET sealed = $2, token_expires_at = $3, status = $4, updated_at = now() WHERE id = $1",
			id, next.sealed, next.expiresAt, next.status); err != nil {
			return fmt.Errorf("store refreshed grant: %w", err)
		}
		return nil
	})
	return cmp.Or(err, fnErr)
}

func (s *store) setStatus(ctx context.Context, id, status string) error {
	_, err := s.pool.Exec(ctx, "UPDATE connect_grants SET status = $2, updated_at = now() WHERE id = $1", id, status)
	return err
}

// setPolicy sets tool's policy in the user's grant of connector; an empty policy drops the user's choice.
func (s *store) setPolicy(ctx context.Context, user, connector, tool, policy string) error {
	sql := "UPDATE connect_grants SET policy = policy || jsonb_build_object($3::text, $4::text), updated_at = now() WHERE user_id = $1 AND connector = $2"
	args := []any{user, connector, tool, policy}
	if policy == "" {
		sql = "UPDATE connect_grants SET policy = policy - $3::text, updated_at = now() WHERE user_id = $1 AND connector = $2"
		args = args[:3]
	}
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("set policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errNoGrant
	}
	return nil
}

func (s *store) deleteGrant(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM connect_grants WHERE id = $1", id)
	return err
}

func (s *store) insertFlow(ctx context.Context, f *flow) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM connect_flows WHERE expires_at < now()"); err != nil {
		return fmt.Errorf("purge flows: %w", err)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO connect_flows (id, user_id, connector, kind, mcp_url, state_sha256, nonce_sha256, session_sha256, sealed, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		f.id, f.user, f.connector, f.kind, f.mcpURL, f.stateSHA256, f.nonceSHA256, f.sessionSHA256, f.sealed, f.expiresAt)
	if err != nil {
		return fmt.Errorf("store flow: %w", err)
	}
	return nil
}

// takeFlow deletes and returns the unexpired flow of state whose nonce matches; a flow is taken once.
func (s *store) takeFlow(ctx context.Context, stateSHA256, nonceSHA256 string) (*flow, error) {
	f := &flow{stateSHA256: stateSHA256, nonceSHA256: nonceSHA256}
	err := s.pool.QueryRow(ctx, `DELETE FROM connect_flows WHERE state_sha256 = $1 AND nonce_sha256 = $2
		RETURNING id, user_id, connector, kind, mcp_url, session_sha256, sealed, expires_at`, stateSHA256, nonceSHA256).
		Scan(&f.id, &f.user, &f.connector, &f.kind, &f.mcpURL, &f.sessionSHA256, &f.sealed, &f.expiresAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !time.Now().Before(f.expiresAt)) {
		return nil, errNoFlow
	}
	if err != nil {
		return nil, fmt.Errorf("take flow: %w", err)
	}
	return f, nil
}

func (s *store) insertApproval(ctx context.Context, a *approval) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO connect_approvals (id, user_id, grant_id, tool, args_sha256, summary, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, a.id, a.user, a.grantID, a.tool, a.argsSHA256, a.summary, a.expiresAt)
	if err != nil {
		return fmt.Errorf("store approval: %w", err)
	}
	return nil
}

func (s *store) decide(ctx context.Context, user, id, decision string) (*approval, error) {
	a := &approval{id: id, user: user}
	err := s.pool.QueryRow(ctx, `UPDATE connect_approvals a SET decision = $3 FROM connect_grants g
		WHERE a.id = $1 AND a.user_id = $2 AND a.decision = 'pending' AND a.expires_at > now() AND g.id = a.grant_id
		RETURNING a.grant_id, g.connector, a.tool, a.summary`, id, user, decision).Scan(&a.grantID, &a.connector, &a.tool, &a.summary)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoApproval
	}
	if err != nil {
		return nil, fmt.Errorf("answer approval: %w", err)
	}
	return a, nil
}

// useApproval spends the approval on one call and returns "", or the reason it does not admit the call.
func (s *store) useApproval(ctx context.Context, id, user, grantID, tool, argsSHA256 string) (string, error) {
	var decision, args string
	var used bool
	var expires time.Time
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT decision, args_sha256, used_at IS NOT NULL, expires_at FROM connect_approvals
			WHERE id = $1 AND user_id = $2 AND grant_id = $3 AND tool = $4 FOR UPDATE`, id, user, grantID, tool).Scan(&decision, &args, &used, &expires)
		if err != nil {
			return err
		}
		if (decision != decisionOnce && decision != decisionAlways) || used || args != argsSHA256 || !time.Now().Before(expires) {
			return nil
		}
		_, err = tx.Exec(ctx, "UPDATE connect_approvals SET used_at = now() WHERE id = $1", id)
		decision = decisionUsedNow
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "the approval is for another call", nil
	case err != nil:
		return "", fmt.Errorf("use approval: %w", err)
	case decision == decisionUsedNow:
		return "", nil
	case used:
		return "the approval was used already", nil
	case !time.Now().Before(expires):
		return "the approval expired", nil
	case args != argsSHA256:
		return "the approval covers other arguments", nil
	case decision == decisionDeny:
		return "the user denied the call", nil
	default:
		return "the user has not answered the approval yet", nil
	}
}

func (s *store) pending(ctx context.Context, user string) ([]*approval, error) {
	rows, err := s.pool.Query(ctx, `SELECT a.id, a.grant_id, g.connector, a.tool, a.summary, a.expires_at FROM connect_approvals a
		JOIN connect_grants g ON g.id = a.grant_id WHERE a.user_id = $1 AND a.decision = 'pending' AND a.expires_at > now() ORDER BY a.expires_at`, user)
	if err != nil {
		return nil, fmt.Errorf("list approvals: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*approval, error) {
		a := &approval{user: user}
		err := row.Scan(&a.id, &a.grantID, &a.connector, &a.tool, &a.summary, &a.expiresAt)
		return a, err
	})
}

// putDoorKey stores the hash of machine's door key for its incarnation in place of its earlier ones.
func (s *store) putDoorKey(ctx context.Context, keySHA256, machine, user, incarnation string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM connect_door_keys WHERE machine_id = $1", machine); err != nil {
			return fmt.Errorf("drop door keys: %w", err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO connect_door_keys (key_sha256, machine_id, user_id, incarnation) VALUES ($1, $2, $3, $4)",
			keySHA256, machine, user, incarnation); err != nil {
			return fmt.Errorf("store door key: %w", err)
		}
		return nil
	})
}

// doorKey returns the user and machine of the door key whose hash is keySHA256.
func (s *store) doorKey(ctx context.Context, keySHA256 string) (string, string, error) {
	var user, machine string
	err := s.pool.QueryRow(ctx, "SELECT user_id, machine_id FROM connect_door_keys WHERE key_sha256 = $1", keySHA256).Scan(&user, &machine)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", errNoDoorKey
	}
	if err != nil {
		return "", "", fmt.Errorf("read door key: %w", err)
	}
	return user, machine, nil
}

func (s *store) dropDoorKeys(ctx context.Context, machine string) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM connect_door_keys WHERE machine_id = $1", machine); err != nil {
		return fmt.Errorf("drop door keys: %w", err)
	}
	return nil
}

func (s *store) pruneDoorKeys(ctx context.Context, live []string, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM connect_door_keys WHERE machine_id <> ALL($1::text[]) AND created_at < $2", live, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune door keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *store) audit(ctx context.Context, r *record) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO connect_audit (user_id, connector, tool, door, caller, decision, outcome, upstream_status, latency_ms, req_bytes, resp_bytes, args_sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		r.user, r.connector, r.tool, r.door, r.caller, r.decision, r.outcome, r.status, r.latency.Milliseconds(), r.reqBytes, r.respBytes, r.argsSHA256)
	return err
}

func (s *store) activity(ctx context.Context, user, connector string, limit int) ([]*record, error) {
	rows, err := s.pool.Query(ctx, `SELECT at, connector, tool, door, caller, decision, outcome, upstream_status, latency_ms, req_bytes, resp_bytes, args_sha256
		FROM connect_audit WHERE user_id = $1 AND ($2 = '' OR connector = $2) ORDER BY at DESC, id DESC LIMIT $3`, user, connector, limit)
	if err != nil {
		return nil, fmt.Errorf("read activity: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*record, error) {
		r := &record{user: user}
		var ms int64
		err := row.Scan(&r.at, &r.connector, &r.tool, &r.door, &r.caller, &r.decision, &r.outcome, &r.status, &ms, &r.reqBytes, &r.respBytes, &r.argsSHA256)
		r.latency = time.Duration(ms) * time.Millisecond
		return r, err
	})
}

func (s *store) purge(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM connect_audit WHERE at < $1", time.Now().Add(-retention))
	if err != nil {
		return 0, fmt.Errorf("purge audit: %w", err)
	}
	if _, err := s.pool.Exec(ctx, "DELETE FROM connect_approvals WHERE expires_at < now() - interval '1 day'"); err != nil {
		return 0, fmt.Errorf("purge approvals: %w", err)
	}
	return tag.RowsAffected(), nil
}

func scanGrant(row pgx.CollectableRow) (*grant, error) {
	g := &grant{}
	err := row.Scan(&g.id, &g.user, &g.connector, &g.kind, &g.mcpURL, &g.account, &g.scopes, &g.status, &g.policy, &g.sealed, &g.expiresAt, &g.createdAt)
	return g, err
}
