// Package clustertest opens a cluster store on a fresh database of the server COCOONBOX_TEST_DSN names.
package clustertest

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loomhand/cocoonbox/api/cluster"
)

// Open returns a store on a fresh database dropped when t ends, and skips t without COCOONBOX_TEST_DSN.
func Open(t testing.TB) *cluster.Store {
	t.Helper()
	st, _ := OpenDSN(t)
	return st
}

// OpenDSN is Open that also returns the database's connection string.
func OpenDSN(t testing.TB) (*cluster.Store, string) {
	t.Helper()
	dsn := EmptyDSN(t)
	st, err := cluster.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open cluster store: %v", err)
	}
	t.Cleanup(st.Close)
	return st, dsn
}

// EmptyDSN returns the dsn of a fresh database without tables, dropped when t ends, and skips t without COCOONBOX_TEST_DSN.
func EmptyDSN(t testing.TB) string {
	t.Helper()
	base := os.Getenv("COCOONBOX_TEST_DSN")
	if base == "" {
		t.Skip("COCOONBOX_TEST_DSN names no PostgreSQL server")
	}
	name := "t_" + strings.ToLower(rand.Text()[:12])
	if err := exec(base, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _ = exec(base, "DROP DATABASE "+name+" WITH (FORCE)") })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse COCOONBOX_TEST_DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// WaitFor fails t unless cond holds within 5 s, as a notify takes effect.
func WaitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 5 s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
