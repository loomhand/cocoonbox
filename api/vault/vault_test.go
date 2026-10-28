package vault

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/utils"
)

const (
	alice = "u-alice"
	bob   = "u-bob"

	synthetic = "sk-proj-synthetic0123456789abcdef"
	rotated   = "sk-proj-rotated9876543210fedcba"
)

func TestKeysLifecycle(t *testing.T) {
	svc := newService(t)
	var changes []string
	svc.OnChange(func(_ context.Context, owner string) { changes = append(changes, owner) })
	ctx := auth.WithUser(t.Context(), alice)

	k, err := svc.Add(ctx, &controlv1.AddKeyRequest{Name: "ATTIO_API_KEY", Hosts: []string{"api.attio.com"}, Value: synthetic})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if k.GetHeader() != "Authorization" || k.GetTemplate() != "Bearer {}" || !strings.HasPrefix(k.GetPlaceholder(), "sk-proj-cbv_") || k.GetHint() != "cdef" {
		t.Errorf("Add = %v", k)
	}
	if _, err := svc.Add(ctx, &controlv1.AddKeyRequest{Name: "ATTIO_API_KEY", Hosts: []string{"api.attio.com"}, Value: synthetic}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("second Add = %v, want AlreadyExists", err)
	}
	if _, err := svc.Add(ctx, &controlv1.AddKeyRequest{Name: "X", Hosts: []string{"*.vercel.app"}, Value: synthetic}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Add on *.vercel.app = %v, want InvalidArgument", err)
	}
	list, err := svc.List(ctx, &controlv1.ListKeysRequest{})
	if err != nil || len(list.GetKeys()) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if raw, _ := protojson.Marshal(list); strings.Contains(string(raw), "synthetic") {
		t.Errorf("List carries the value: %s", raw)
	}
	if other, _ := svc.List(auth.WithUser(t.Context(), bob), &controlv1.ListKeysRequest{}); len(other.GetKeys()) != 0 {
		t.Errorf("bob lists %v", other)
	}

	creds, d1, err := svc.Credentials(t.Context(), alice)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	name := onlyName(t, creds.Secrets)
	sec := creds.Secrets[name]
	if sec.Value != "Bearer "+synthetic || sec.Placeholder != "Bearer "+k.GetPlaceholder() || !slices.Equal(sec.Hosts, []string{"api.attio.com"}) ||
		creds.Prune != envPrefix || creds.Flags[guestEnv] != "ATTIO_API_KEY="+k.GetPlaceholder()+";api.attio.com;Authorization:%20Bearer%20{}" {
		t.Errorf("Credentials = %+v %+v", sec, creds)
	}
	if digests, _ := svc.Digests(t.Context()); digests[alice] != d1 || d1 == "" || digests[bob] != "" {
		t.Errorf("Digests = %v, Credentials digest %q", digests, d1)
	}

	r, err := svc.Rotate(ctx, &controlv1.RotateKeyRequest{Name: "ATTIO_API_KEY", Value: rotated})
	if err != nil || r.GetPlaceholder() != k.GetPlaceholder() || r.GetHint() != "dcba" {
		t.Fatalf("Rotate = %v, %v", r, err)
	}
	creds, d2, _ := svc.Credentials(t.Context(), alice)
	if creds.Secrets[name].Value != "Bearer "+rotated || d2 == d1 {
		t.Errorf("after Rotate: %+v, digest %q (was %q)", creds.Secrets[name], d2, d1)
	}
	if _, err := svc.Rotate(ctx, &controlv1.RotateKeyRequest{Name: "NOPE", Value: rotated}); status.Code(err) != codes.NotFound {
		t.Errorf("Rotate of an unknown key = %v, want NotFound", err)
	}

	if _, err := svc.Delete(WithActor(ctx, ActorConsole), &controlv1.KeyRef{Name: "ATTIO_API_KEY"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	creds, d3, _ := svc.Credentials(t.Context(), alice)
	if len(creds.Secrets) != 0 || creds.Flags[guestEnv] != "" || creds.Prune != envPrefix || d3 != "" {
		t.Errorf("after Delete: %+v, digest %q", creds, d3)
	}
	if !slices.Equal(changes, []string{alice, alice, alice}) {
		t.Errorf("changes = %v, want three of alice", changes)
	}
	rows, _ := svc.store.pool.Query(t.Context(), "SELECT op || ':' || actor FROM vault_audit WHERE owner = $1 ORDER BY id", alice)
	ops, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	if !slices.Equal(ops, []string{"create:app", "rotate:app", "delete:console"}) {
		t.Errorf("audit = %v", ops)
	}
}

func TestQueryKeyAndTheCountLimit(t *testing.T) {
	svc := newService(t)
	ctx := auth.WithUser(t.Context(), alice)
	if _, err := svc.Add(ctx, &controlv1.AddKeyRequest{Name: "SERP", Hosts: []string{"serpapi.com"}, Query: "api_key", Value: "0123456789abcdef"}); err != nil {
		t.Fatalf("Add query key: %v", err)
	}
	creds, _, _ := svc.Credentials(t.Context(), alice)
	if sec := creds.Secrets[onlyName(t, creds.Secrets)]; sec.Query != "api_key" || sec.Value != "0123456789abcdef" || !strings.HasPrefix(sec.Placeholder, "cbv_") {
		t.Errorf("query entry = %+v", sec)
	}
	for i := range maxKeys - 1 {
		if _, err := svc.Add(ctx, &controlv1.AddKeyRequest{Name: fmt.Sprintf("K%d", i), Hosts: []string{"a.example.com"}, Value: synthetic}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if _, err := svc.Add(ctx, &controlv1.AddKeyRequest{Name: "K33", Hosts: []string{"a.example.com"}, Value: synthetic}); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("33rd Add = %v, want ResourceExhausted", err)
	}
}

func TestASealedValueOpensOnlyOnItsOwnRow(t *testing.T) {
	svc := newService(t)
	for _, owner := range []string{alice, bob} {
		if _, err := svc.Add(auth.WithUser(t.Context(), owner), &controlv1.AddKeyRequest{Name: "K", Hosts: []string{"a.example.com"}, Value: synthetic}); err != nil {
			t.Fatalf("Add for %s: %v", owner, err)
		}
	}
	if _, err := svc.store.pool.Exec(t.Context(), `UPDATE vault_secrets SET value_sealed = (SELECT value_sealed FROM vault_secrets WHERE owner = $1)
		WHERE owner = $2`, alice, bob); err != nil {
		t.Fatalf("copy the sealed value: %v", err)
	}
	if _, _, err := svc.Credentials(t.Context(), bob); err == nil {
		t.Error("a sealed value copied to another user's row opened")
	}
	if _, _, err := svc.Credentials(t.Context(), alice); err != nil {
		t.Errorf("the owner's row stopped opening: %v", err)
	}
}

func TestDeletingAUserDropsItsKeysAndAudit(t *testing.T) {
	svc := newService(t)
	if _, err := svc.Add(auth.WithUser(t.Context(), alice), &controlv1.AddKeyRequest{Name: "K", Hosts: []string{"a.example.com"}, Value: synthetic}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := svc.store.pool.Exec(t.Context(), "DELETE FROM users WHERE id = $1", alice); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var keys, audit int
	if err := svc.store.pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM vault_secrets), (SELECT count(*) FROM vault_audit)").Scan(&keys, &audit); err != nil || keys+audit != 0 {
		t.Errorf("after the user went: %d keys, %d audit rows, %v", keys, audit, err)
	}
}

func TestKeysAnswerAStoreFailureWithoutItsText(t *testing.T) {
	svc := newService(t)
	svc.store.pool.Close()
	ctx := auth.WithUser(t.Context(), alice)
	_, listErr := svc.List(ctx, &controlv1.ListKeysRequest{})
	_, addErr := svc.Add(ctx, &controlv1.AddKeyRequest{Name: "K", Hosts: []string{"a.example.com"}, Value: synthetic})
	_, deleteErr := svc.Delete(ctx, &controlv1.KeyRef{Name: "K"})
	for _, err := range []error{listErr, addErr, deleteErr} {
		if st := status.Convert(err); st.Code() != codes.Internal || !strings.HasSuffix(st.Message(), " failed") || strings.Contains(st.Message(), "pool") {
			t.Errorf("err = %v, want Internal with a generic message", err)
		}
	}
}

func onlyName(t *testing.T, m map[string]*machines.Secret) string {
	t.Helper()
	if len(m) != 1 {
		t.Fatalf("secrets = %v, want one", m)
	}
	for name := range m {
		if !strings.HasPrefix(name, envPrefix) {
			t.Errorf("entry %q lacks %s", name, envPrefix)
		}
		return name
	}
	return ""
}

func newService(t *testing.T) *Service {
	t.Helper()
	dsn := freshDatabase(t)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(t.Context(), "CREATE TABLE users (id text PRIMARY KEY); INSERT INTO users VALUES ('u-alice'), ('u-bob')"); err != nil {
		t.Fatalf("users table: %v", err)
	}
	svc, err := Open(t.Context(), Options{DSN: dsn, Keys: testKeys{}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

func freshDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("COCOONBOX_TEST_DSN")
	if base == "" {
		t.Skip("COCOONBOX_TEST_DSN names no PostgreSQL server")
	}
	conn, err := pgx.Connect(t.Context(), base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	name := "t_" + utils.RandomHex(6)
	if _, err := conn.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), base); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse COCOONBOX_TEST_DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

type testKeys struct{}

func (testKeys) StateKey(_ context.Context, user string) (keyring.Key, error) {
	secret := make([]byte, keyring.KeySize)
	copy(secret, user)
	return keyring.Key{ID: "test:" + user, Secret: secret}, nil
}
