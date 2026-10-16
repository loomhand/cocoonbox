// Package migrate applies numbered SQL files embedded in a binary to PostgreSQL, one replica at a time.
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open connects to dsn and runs fsys's migrations/<n>_<name>.sql not yet in table, in number order, under the advisory lock.
func Open(ctx context.Context, dsn string, fsys fs.FS, table string, lock int64) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := apply(ctx, pool, fsys, table, lock); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func apply(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, table string, lock int64) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lock) }()
	if _, err = conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+" (version integer PRIMARY KEY)"); err != nil {
		return fmt.Errorf("create %s: %w", table, err)
	}
	names, err := fs.Glob(fsys, "migrations/*.sql")
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
		if err = conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE version = $1)", version).Scan(&applied); err != nil {
			return fmt.Errorf("read %s: %w", table, err)
		}
		if applied {
			continue
		}
		sql, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO "+table+" (version) VALUES ($1)", version)
			return err
		}); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}
