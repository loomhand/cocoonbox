package migrate_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/migrate"
)

func TestPendingListsWhatOpenAppliesAndWritesNothing(t *testing.T) {
	dsn := clustertest.EmptyDSN(t)
	set := migrate.Set{FS: fstest.MapFS{
		"migrations/0001_a.sql": {Data: []byte("CREATE TABLE a (x int)")},
		"migrations/0002_b.sql": {Data: []byte("CREATE TABLE b (x int)")},
	}, Table: "t_schema_migrations", Lock: 1}
	pending, err := migrate.Pending(t.Context(), dsn, set)
	if err != nil || !slices.Equal(pending, []string{"migrations/0001_a.sql", "migrations/0002_b.sql"}) {
		t.Fatalf("Pending on an empty database = %v, %v", pending, err)
	}
	if tables := tableCount(t, dsn); tables != 0 {
		t.Fatalf("tables after Pending = %d, want none", tables)
	}
	pool, err := migrate.Open(t.Context(), dsn, set)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	pool.Close()
	if pending, err := migrate.Pending(t.Context(), dsn, set); err != nil || len(pending) != 0 {
		t.Errorf("Pending after Open = %v, %v; want none", pending, err)
	}
}

func TestARoleWithoutDDLOpensAMigratedDatabaseAndFailsOnAPendingMigration(t *testing.T) {
	dsn := clustertest.EmptyDSN(t)
	files := fstest.MapFS{"migrations/0001_a.sql": {Data: []byte("CREATE TABLE a (x int)")}}
	set := migrate.Set{FS: files, Table: "t_schema_migrations", Lock: 1}
	if err := migrate.Apply(t.Context(), dsn, set); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	role := "t_dml_" + strings.ToLower(rand.Text()[:10])
	admin(t, dsn, `CREATE ROLE `+role+` LOGIN PASSWORD 'dml'`)
	t.Cleanup(func() {
		admin(t, dsn, "DROP OWNED BY "+role)
		admin(t, dsn, "DROP ROLE "+role)
	})
	admin(t, dsn, "REVOKE CREATE ON SCHEMA public FROM PUBLIC")
	admin(t, dsn, "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO "+role)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(role, "dml")
	pool, err := migrate.Open(t.Context(), u.String(), set)
	if err != nil {
		t.Fatalf("Open of a migrated database as a role without DDL: %v", err)
	}
	pool.Close()
	files["migrations/0002_b.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE b (x int)")}
	if _, err := migrate.Open(t.Context(), u.String(), set); err == nil || !strings.Contains(err.Error(), "0002_b") {
		t.Errorf("Open with a pending migration as a role without DDL = %v, want its failure", err)
	}
	if pending, err := migrate.Pending(t.Context(), dsn, set); err != nil || !slices.Equal(pending, []string{"migrations/0002_b.sql"}) {
		t.Errorf("Pending after the refused Open = %v, %v", pending, err)
	}
}

func admin(t *testing.T, dsn, sql string) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func tableCount(t *testing.T, dsn string) int {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var n int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'").Scan(&n); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	return n
}
