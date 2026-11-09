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

// Set is one store's migrations: FS's migrations/<n>_<name>.sql, the table that records the applied ones and the advisory lock their runs take.
type Set struct {
	FS    fs.FS
	Table string
	Lock  int64
}

// Open connects to dsn and runs set's pending migrations, in number order, under its advisory lock; with none pending it writes nothing, so a role without DDL rights opens a migrated database.
func Open(ctx context.Context, dsn string, set Set) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := apply(ctx, pool, set); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Apply connects to dsn and runs the pending migrations of each set in turn.
func Apply(ctx context.Context, dsn string, sets ...Set) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	for _, set := range sets {
		if err := apply(ctx, pool, set); err != nil {
			return err
		}
	}
	return nil
}

// Pending connects to dsn and returns set's migrations not yet in its table, writing nothing.
func Pending(ctx context.Context, dsn string, set Set) ([]string, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var exists bool
	if err = conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", set.Table).Scan(&exists); err != nil {
		return nil, fmt.Errorf("read %s: %w", set.Table, err)
	}
	applied := map[int]bool{}
	if exists {
		var rows pgx.Rows
		if rows, err = conn.Query(ctx, "SELECT version FROM "+set.Table); err != nil {
			return nil, fmt.Errorf("read %s: %w", set.Table, err)
		}
		var versions []int
		if versions, err = pgx.CollectRows(rows, pgx.RowTo[int]); err != nil {
			return nil, fmt.Errorf("read %s: %w", set.Table, err)
		}
		for _, v := range versions {
			applied[v] = true
		}
	}
	names, err := migrations(set.FS)
	if err != nil {
		return nil, err
	}
	var pending []string
	for _, name := range names {
		version, err := versionOf(name)
		if err != nil {
			return nil, err
		}
		if !applied[version] {
			pending = append(pending, name)
		}
	}
	return pending, nil
}

func apply(ctx context.Context, pool *pgxpool.Pool, set Set) error {
	table := set.Table
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock($1)", set.Lock); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", set.Lock) }()
	var exists bool
	if err = conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
		return fmt.Errorf("read %s: %w", table, err)
	}
	if !exists {
		if _, err = conn.Exec(ctx, "CREATE TABLE "+table+" (version integer PRIMARY KEY)"); err != nil {
			return fmt.Errorf("create %s: %w", table, err)
		}
	}
	names, err := migrations(set.FS)
	if err != nil {
		return err
	}
	for _, name := range names {
		version, err := versionOf(name)
		if err != nil {
			return err
		}
		var applied bool
		if err = conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE version = $1)", version).Scan(&applied); err != nil {
			return fmt.Errorf("read %s: %w", table, err)
		}
		if applied {
			continue
		}
		sql, err := fs.ReadFile(set.FS, name)
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

func migrations(fsys fs.FS) ([]string, error) {
	names, err := fs.Glob(fsys, "migrations/*.sql")
	slices.Sort(names)
	return names, err
}

func versionOf(name string) (int, error) {
	number, _, _ := strings.Cut(strings.TrimPrefix(name, "migrations/"), "_")
	version, err := strconv.Atoi(number)
	if err != nil {
		return 0, fmt.Errorf("migration %s: name must start with its number", name)
	}
	return version, nil
}
