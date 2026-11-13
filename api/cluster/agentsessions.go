package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const agentSessionIdle = 24 * time.Hour

// AgentSession returns the agent session client holds on box, empty without one, and marks it read.
func (s *Store) AgentSession(ctx context.Context, client, box string) (string, error) {
	var upstream string
	err := s.pool.QueryRow(ctx, "UPDATE agent_sessions SET read_at = now() WHERE client = $1 AND box = $2 RETURNING upstream", client, box).Scan(&upstream)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the agent session of %s: %w", box, err)
	}
	return upstream, nil
}

// PutAgentSession records upstream as client's agent session on box unless another process recorded one first, and returns the one kept.
func (s *Store) PutAgentSession(ctx context.Context, client, box, upstream string) (string, error) {
	if _, err := s.pool.Exec(ctx, "DELETE FROM agent_sessions WHERE read_at < $1", time.Now().Add(-agentSessionIdle)); err != nil {
		return "", fmt.Errorf("purge idle agent sessions: %w", err)
	}
	var kept string
	if err := s.pool.QueryRow(ctx, `INSERT INTO agent_sessions (client, box, upstream) VALUES ($1, $2, $3)
		ON CONFLICT (client, box) DO UPDATE SET read_at = now() RETURNING upstream`, client, box, upstream).Scan(&kept); err != nil {
		return "", fmt.Errorf("record the agent session of %s: %w", box, err)
	}
	return kept, nil
}

// DropAgentSession forgets client's agent session on box while it is still upstream.
func (s *Store) DropAgentSession(ctx context.Context, client, box, upstream string) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM agent_sessions WHERE client = $1 AND box = $2 AND upstream = $3", client, box, upstream); err != nil {
		return fmt.Errorf("drop the agent session of %s: %w", box, err)
	}
	return nil
}

// DropAgentSessions forgets every agent session of client and returns them, keyed by box.
func (s *Store) DropAgentSessions(ctx context.Context, client string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, "DELETE FROM agent_sessions WHERE client = $1 RETURNING box, upstream", client)
	if err != nil {
		return nil, fmt.Errorf("drop the agent sessions of a client: %w", err)
	}
	out := map[string]string{}
	var box, upstream string
	if _, err = pgx.ForEachRow(rows, []any{&box, &upstream}, func() error {
		out[box] = upstream
		return nil
	}); err != nil {
		return nil, fmt.Errorf("drop the agent sessions of a client: %w", err)
	}
	return out, nil
}
