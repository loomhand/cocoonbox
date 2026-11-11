package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/users"
	"github.com/loomhand/cocoonbox/api/users/userstest"
	"github.com/loomhand/cocoonbox/api/utils"
)

const agentClient = "agent"

func TestEndingAGrantEndsItsCallsInAnotherApiWhenNoNotifyCanFollowTheWrite(t *testing.T) {
	tests := []struct {
		name string
		end  func(t *testing.T, s *Service, user string, g grantTokens)
	}{
		{
			name: "revoke",
			end: func(t *testing.T, s *Service, user string, g grantTokens) {
				if err := s.Revoke(t.Context(), user, g.id); err != nil {
					t.Fatalf("Revoke: %v", err)
				}
			},
		},
		{
			name: "revoke all",
			end: func(t *testing.T, s *Service, user string, _ grantTokens) {
				if err := s.RevokeAll(t.Context(), user); err != nil {
					t.Fatalf("RevokeAll: %v", err)
				}
			},
		},
		{
			name: "revoke by token",
			end: func(t *testing.T, s *Service, _ string, g grantTokens) {
				if code := postForm(t, s.ServeRevoke, url.Values{"token": {g.access}, "client_id": {agentClient}}); code != http.StatusOK {
					t.Fatalf("revoke endpoint answered %d", code)
				}
			},
		},
		{
			name: "refresh replay",
			end: func(t *testing.T, s *Service, _ string, g grantTokens) {
				form := url.Values{"grant_type": {refreshToken}, refreshToken: {g.refresh}, "client_id": {agentClient}}
				if code := postForm(t, s.ServeToken, form); code != http.StatusOK {
					t.Fatalf("the first refresh answered %d", code)
				}
				if code := postForm(t, s.ServeToken, form); code != http.StatusBadRequest {
					t.Fatalf("the replayed refresh answered %d", code)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn := clustertest.EmptyDSN(t)
			writer := openUsers(t, dsn)
			alice, err := writer.Create(t.Context(), users.NewUser{Name: "alice"})
			if err != nil {
				t.Fatalf("create alice: %v", err)
			}
			listener := openUsers(t, dsn)
			go listener.Listen(t.Context())
			bob, err := writer.Create(t.Context(), users.NewUser{Name: "bob"})
			if err != nil {
				t.Fatalf("create bob: %v", err)
			}
			clustertest.WaitFor(t, "the other api listens", func() bool { _, err := listener.Get(bob.ID); return err == nil })
			s, err := Open(t.Context(), Options{DSN: dsn, Issuer: "https://cb.example", Resource: "https://cb.example/mcp", Accounts: lostEnd{writer}})
			if err != nil {
				t.Fatalf("open oauth: %v", err)
			}
			t.Cleanup(s.Close)
			cfg, err := pgxpool.ParseConfig(dsn)
			if err != nil {
				t.Fatalf("parse dsn: %v", err)
			}
			cfg.ConnConfig.Tracer = userstest.LateNotifyFails{}
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatalf("open traced pool: %v", err)
			}
			s.store.pool.Close()
			s.store.pool = pool
			g := issueGrant(t, s, alice.ID)
			calls, done := listener.Lifetime(alice.ID, g.id)
			t.Cleanup(done)
			if calls.Err() != nil {
				t.Fatal("the grant's calls ended before the revoke")
			}
			tt.end(t, s, alice.ID, g)
			clustertest.WaitFor(t, "the other api ends the grant's calls", func() bool { return calls.Err() != nil })
		})
	}
}

func TestARevokeByTokenThatRollsBackNamesNoGrant(t *testing.T) {
	dsn := clustertest.EmptyDSN(t)
	writer := openUsers(t, dsn)
	alice, err := writer.Create(t.Context(), users.NewUser{Name: "alice"})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	s, err := Open(t.Context(), Options{DSN: dsn, Issuer: "https://cb.example", Resource: "https://cb.example/mcp", Accounts: writer})
	if err != nil {
		t.Fatalf("open oauth: %v", err)
	}
	t.Cleanup(s.Close)
	g := issueGrant(t, s, alice.ID)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Tracer = notifyFails{}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	s.store.pool.Close()
	s.store.pool = pool
	if id, err := s.store.revokeByToken(t.Context(), utils.HashHex(g.access), agentClient); id != "" || err == nil {
		t.Fatalf("revokeByToken = %q, %v; want no grant and the rollback's error", id, err)
	}
	if grants, err := s.store.grants(t.Context(), alice.ID); err != nil || len(grants) != 1 {
		t.Errorf("grants after the rolled-back revoke = %d, %v; want the grant kept", len(grants), err)
	}
}

type grantTokens struct {
	id, access, refresh string
}

type lostEnd struct {
	*users.Service
}

func (lostEnd) EndCredential(context.Context, string) {}

type notifyFails struct{}

func (notifyFails) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(data.SQL, "pg_notify") {
		return ctx
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	return canceled
}

func (notifyFails) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func openUsers(t *testing.T, dsn string) *users.Service {
	t.Helper()
	us, err := users.Open(t.Context(), users.Options{
		DSN: dsn, MasterKeyFile: userstest.MasterKeyFile(t), GW: userstest.NewGW(), Tenants: userstest.NewTenants(),
		FirstBoxID: func(user string) string { return "cb-" + utils.HashHex(user)[:12] }, EnsureLimit: 4, PasswordCost: bcrypt.MinCost,
	})
	if err != nil {
		t.Fatalf("open users: %v", err)
	}
	t.Cleanup(us.Close)
	return us
}

func issueGrant(t *testing.T, s *Service, user string) grantTokens {
	t.Helper()
	g := grantTokens{id: grantPrefix + utils.RandomHex(grantIDBytes), access: randomToken(AccessPrefix), refresh: randomToken(refreshPrefix)}
	c := code{
		user: user, client: client{id: agentClient}, caps: Caps{MaxBoxes: 1, MaxRunning: 1}, chosen: []string{},
		grantExpires: time.Now().Add(time.Hour), expiresAt: time.Now().Add(time.Minute), codeSHA256: utils.HashHex(randomToken("code_")),
	}
	if err := s.store.addCode(t.Context(), c); err != nil {
		t.Fatalf("addCode: %v", err)
	}
	if _, err := s.store.redeem(t.Context(), c.codeSHA256, g.id, issued{access: utils.HashHex(g.access), refresh: utils.HashHex(g.refresh)}, func(code) error { return nil }); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return g
}

func postForm(t *testing.T, serve http.HandlerFunc, form url.Values) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	serve(rec, req)
	return rec.Code
}
