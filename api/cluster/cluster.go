// Package cluster keeps the state api processes share in PostgreSQL: machines, box heads and operations.
package cluster

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/migrate"
)

const (
	migrationLock = 0x6362636c // "cbcl"

	ChannelMachines   = "cocoonbox_machines"
	ChannelHeads      = "cocoonbox_heads"
	ChannelOperations = "cocoonbox_operations"

	listenRetry = 5 * time.Second
	lockConns   = 4
)

var (
	// ErrNotFound reports a missing row.
	ErrNotFound = errors.New("cluster row not found")
	// ErrStale reports a conditional write that lost to a write after the read.
	ErrStale = errors.New("cluster row changed since it was read")

	//go:embed migrations/*.sql
	migrations embed.FS
)

// Store is the shared state in one PostgreSQL database.
type Store struct {
	pool *pgxpool.Pool
	// locks holds the connections of the user locks.
	locks *pgxpool.Pool
}

// Open connects to dsn and applies the cluster migrations.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := migrate.Open(ctx, dsn, migrations, "cluster_schema_migrations", migrationLock)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("parse the database dsn: %w", err)
	}
	cfg.MaxConns = lockConns
	locks, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("open the lock pool: %w", err)
	}
	return &Store{pool: pool, locks: locks}, nil
}

// Close closes the store; closing a nil store does nothing.
func (s *Store) Close() {
	if s != nil {
		s.locks.Close()
		s.pool.Close()
	}
}

// Listen calls fn with each notify payload on channel until ctx ends, and reset after each (re)connect since a missed notify is lost.
func (s *Store) Listen(ctx context.Context, channel string, reset func(), fn func(payload string)) {
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

func (s *Store) listen(ctx context.Context, channel string, reset func(), fn func(payload string)) error {
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
