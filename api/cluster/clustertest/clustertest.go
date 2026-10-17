// Package clustertest opens a cluster store on a fresh database of the server COCOONBOX_TEST_DSN names.
package clustertest

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/loomhand/cocoonbox/api/cluster"
)

// Open returns a store on a fresh database dropped when t ends, and skips t without COCOONBOX_TEST_DSN.
func Open(t testing.TB) *cluster.Store {
	t.Helper()
	base := os.Getenv("COCOONBOX_TEST_DSN")
	if base == "" {
		t.Skip("COCOONBOX_TEST_DSN names no PostgreSQL server")
	}
	name := "t_" + strings.ToLower(rand.Text()[:12])
	if err := exec(base, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse COCOONBOX_TEST_DSN: %v", err)
	}
	u.Path = "/" + name
	st, err := cluster.Open(t.Context(), u.String())
	if err != nil {
		t.Fatalf("open cluster store: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		_ = exec(base, "DROP DATABASE "+name+" WITH (FORCE)")
	})
	return st
}

func exec(dsn, sql string) error {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, sql)
	return err
}
