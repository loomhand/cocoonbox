// Package cluster keeps the state api processes share in PostgreSQL: machines, box heads, operations and client reaches, with the leader and user locks.
package cluster

import (
	"context"
	"embed"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/migrate"
)

const (
	ChannelMachines   = "cocoonbox_machines"
	ChannelHeads      = "cocoonbox_heads"
	ChannelOperations = "cocoonbox_operations"

	// Lease bounds how long a machine op or an operation outlives its process, which renews it every LeaseRenew while it runs.
	Lease      = time.Minute
	LeaseRenew = 20 * time.Second

	schemaTable   = "cluster_schema_migrations"
	migrationLock = 0x6362636c // "cbcl"
	listenRetry   = 5 * time.Second
	lockConns     = 4
)

var (
	// ErrNotFound reports a missing row.
	ErrNotFound = errors.New("cluster row not found")
	// ErrStale reports a conditional write that lost to a write after the read.
	ErrStale = errors.New("cluster row changed since it was read")

	//go:embed migrations/*.sql
	migrations embed.FS
)

// Notified takes the payload of one notify.
type Notified func(payload string)

// Change is what a notify on ChannelMachines or ChannelOperations carries: the row, a machine's new version, 0 once removed, and whether an operation finished.
type Change struct {
	ID       string
	Version  int64 `json:",omitzero"`
	Finished bool  `json:",omitzero"`
}

// Store is the shared state in one PostgreSQL database.
type Store struct {
	pool *pgxpool.Pool
	// locks holds the connections of the user locks apart, with keepalives so the server frees the locks of a host that died.
	locks  *pgxpool.Pool
	leader *pgx.ConnConfig
}

// Open connects to dsn and applies the cluster migrations.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := migrate.Open(ctx, dsn, migrations, schemaTable, migrationLock)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("parse the database dsn: %w", err)
	}
	cfg.MaxConns = lockConns
	maps.Copy(cfg.ConnConfig.RuntimeParams, map[string]string{"tcp_keepalives_idle": "5", "tcp_keepalives_interval": "2", "tcp_keepalives_count": "3"})
	locks, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("open the lock pool: %w", err)
	}
	return &Store{pool: pool, locks: locks, leader: cfg.ConnConfig.Copy()}, nil
}

// Close closes the store; closing a nil store does nothing.
func (s *Store) Close() {
	if s != nil {
		s.locks.Close()
		s.pool.Close()
	}
}

// Listen calls fn with each notify payload on channel until ctx ends, and reset after each (re)connect since a missed notify is lost.
func (s *Store) Listen(ctx context.Context, channel string, reset func(), fn Notified) {
	for {
		err := s.listen(ctx, channel, reset, fn)
		if ctx.Err() != nil {
			return
		}
		log.WithFunc("cluster.Store.Listen").Warnf(ctx, "listen on %s: %v", channel, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(listenRetry):
		}
	}
}

func (s *Store) listen(ctx context.Context, channel string, reset func(), fn Notified) error {
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire a connection: %w", err)
	}
	conn := pooled.Hijack()
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	reset()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		fn(n.Payload)
	}
}

// ParseChange returns the change a notify on ChannelMachines or ChannelOperations carries.
func ParseChange(payload string) (Change, error) {
	var c Change
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		return Change{}, fmt.Errorf("decode a notify: %w", err)
	}
	return c, nil
}

// PendingMigrations connects to dsn and returns the cluster migrations it lacks, applying none.
func PendingMigrations(ctx context.Context, dsn string) ([]string, error) {
	return migrate.Pending(ctx, dsn, migrations, schemaTable)
}

func notifyChange(ctx context.Context, tx pgx.Tx, channel string, c Change) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode a notify of %s: %w", c.ID, err)
	}
	return notify(ctx, tx, channel, string(raw))
}

func notify(ctx context.Context, tx pgx.Tx, channel, payload string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload); err != nil {
		return fmt.Errorf("notify %s: %w", channel, err)
	}
	return nil
}
