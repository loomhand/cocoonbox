package users

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestLoadCannotRestoreRevokedTokens(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local revoke", true: "newer reload"}[remote], func(t *testing.T) {
			svc, gw, tenants, dsn := newTestService(t)
			u := mustCreate(t, svc, NewUser{Name: "alice"})
			token, err := svc.RegenerateToken(t.Context(), u.ID)
			if err != nil {
				t.Fatal(err)
			}
			writer := svc
			if remote {
				writer = openTestService(t, dsn, gw, tenants)
			}
			gate := gateQuery(t, svc, dsn, "FROM user_tokens ORDER BY")
			done := make(chan error, 1)
			go func() { done <- svc.Load(t.Context()) }()
			<-gate.entered
			err = writer.RevokeToken(t.Context(), u.ID)
			if err == nil && remote {
				err = svc.Load(t.Context())
			}
			close(gate.resume)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
				t.Fatal("the delayed snapshot restored the revoked token")
			}
			if lifetime(svc, u.ID, sum(token)).Err() == nil {
				t.Fatal("the revoked token opened a new lifetime")
			}
		})
	}
}

func TestAnUpdateCannotRestoreATokenRevokedWhileItWaited(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "alice"})
	token, err := svc.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate := gateQuery(t, svc, dsn, "UPDATE users SET max_boxes")
	done := make(chan error, 1)
	go func() {
		_, err := svc.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(2)})
		done <- err
	}()
	<-gate.entered
	err = svc.RevokeToken(t.Context(), u.ID)
	close(gate.resume)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
		t.Fatal("the settings update restored the revoked token")
	}
}

func TestAnIssuedDeviceCannotOutliveASignOutWhileItWaited(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	u := mustCreate(t, svc, NewUser{Name: "alice"})
	gate := gateQuery(t, svc, dsn, "SELECT pg_notify")
	done := make(chan error, 1)
	var token string
	go func() {
		var err error
		token, err = svc.IssueDevice(t.Context(), u.ID, "app", "laptop")
		done <- err
	}()
	<-gate.entered
	err := svc.SignOut(t.Context(), u.ID)
	close(gate.resume)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
		t.Fatal("the delayed issue restored the signed-out device")
	}
}

type reloadGate struct {
	sql     string
	entered chan struct{}
	resume  chan struct{}
	blocked atomic.Bool
}

func (g *reloadGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, g.sql) && g.blocked.CompareAndSwap(false, true) {
		close(g.entered)
		<-g.resume
	}
	return ctx
}

func (*reloadGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func gateQuery(t *testing.T, svc *Service, dsn, sql string) *reloadGate {
	t.Helper()
	return trace(t, svc, dsn, &reloadGate{sql: sql, entered: make(chan struct{}), resume: make(chan struct{})})
}
