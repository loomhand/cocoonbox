// Package userstest holds the gw, tenants, master key and database fixtures the api tests share.
package userstest

import (
	"cmp"
	"context"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loomhand/cocoonbox/api/gwkeys"
	"github.com/loomhand/cocoonbox/api/utils"
)

type GW struct {
	mu       sync.Mutex
	Revoked  map[string]bool
	Caps     map[string]int64
	Disabled map[string]bool
	Machines map[string]bool
	Since    string
}

func NewGW() *GW {
	return &GW{Revoked: map[string]bool{}, Caps: map[string]int64{}, Disabled: map[string]bool{}, Machines: map[string]bool{}}
}

func (g *GW) Revoke(_ context.Context, owner string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Revoked[owner] = true
	return nil
}

func (g *GW) SetEnabled(_ context.Context, owner string, enabled bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Disabled[owner] = !enabled
	return nil
}

func (g *GW) Usage(_ context.Context, _ string, since time.Time) ([]gwkeys.Usage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Since += strconv.FormatInt(since.Unix(), 10) + "|"
	return []gwkeys.Usage{{Model: "m1", Requests: 1, TotalTokens: 10, CostMicros: 5}}, nil
}

func (g *GW) DailyCap(_ context.Context, owner string) (*int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if micros, ok := g.Caps[owner]; ok {
		return &micros, nil
	}
	return nil, nil
}

func (g *GW) DailyCaps(context.Context) (map[string]int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.Caps), nil
}

func (g *GW) SetDailyCap(_ context.Context, owner string, micros *int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if micros == nil {
		delete(g.Caps, owner)
	} else {
		g.Caps[owner] = *micros
	}
	return nil
}

func (g *GW) AddMachine(_ context.Context, owner string, enabled bool) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := "ak-" + utils.RandomHex(4)
	g.Machines[id] = enabled
	return "gw-" + owner + "-" + id, id, nil
}

func (g *GW) Delete(_ context.Context, id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.Machines, id)
	return nil
}

func (g *GW) Cap(owner string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Caps[owner]
}

func (g *GW) Off(owner string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Disabled[owner]
}

type Tenants struct {
	mu     sync.Mutex
	Max    map[string]int
	Tokens map[string]string
	Err    error
}

func NewTenants() *Tenants {
	return &Tenants{Max: map[string]int{}, Tokens: map[string]string{}}
}

func (f *Tenants) PutTenant(_ context.Context, name, token string, maxClaims int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.Max[name], f.Tokens[name] = maxClaims, cmp.Or(token, f.Tokens[name])
	return nil
}

func (f *Tenants) DeleteTenant(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Max, name)
	delete(f.Tokens, name)
	return nil
}

type LateNotifyFails struct{}

func (LateNotifyFails) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "pg_notify") && conn.PgConn().TxStatus() == 'I' {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return canceled
	}
	return ctx
}

func (LateNotifyFails) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func MasterKeyFile(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return path
}

func Query(t testing.TB, dsn, sql string, args ...any) pgx.Row {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn.QueryRow(t.Context(), sql, args...)
}

func Exec(t testing.TB, dsn, sql string, args ...any) error {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(t.Context()) }()
	_, err = conn.Exec(t.Context(), sql, args...)
	return err
}
