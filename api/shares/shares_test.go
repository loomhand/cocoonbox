package shares

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/boxes"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/users"
)

func TestCreateSharesOnlyTheCallersBox(t *testing.T) {
	store := &fakeStore{}
	srv := New(store)
	alice := auth.WithUser(t.Context(), "alice")
	if _, err := srv.Create(t.Context(), &controlv1.CreateShareRequest{BoxId: boxes.BoxID("alice")}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Create without a user: %v", err)
	}
	if _, err := srv.Create(alice, &controlv1.CreateShareRequest{BoxId: boxes.BoxID("bob")}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Create on bob's box: %v", err)
	}
	got, err := srv.Create(alice, &controlv1.CreateShareRequest{BoxId: boxes.BoxID("alice"), TtlSeconds: 600, UserNames: []string{"bob"}, Write: true})
	if err != nil || got.GetSecret() != "cbs_x" || got.GetShare().GetUsers()[0].GetName() != "bob" {
		t.Fatalf("Create = %v, %v", got, err)
	}
	if store.created.Owner != "alice" || store.created.TTL != 10*time.Minute || !store.created.Write {
		t.Errorf("store got %+v", store.created)
	}
}

func TestStoreErrorsBecomeStatuses(t *testing.T) {
	alice := auth.WithUser(t.Context(), "alice")
	for err, want := range map[error]codes.Code{users.ErrInvalid: codes.InvalidArgument, users.ErrExists: codes.AlreadyExists, users.ErrNotFound: codes.NotFound} {
		srv := New(&fakeStore{err: err})
		if _, got := srv.SetWrite(alice, &controlv1.SetShareWriteRequest{ShareId: "sh-1"}); status.Code(got) != want {
			t.Errorf("%v became %v, want %s", err, got, want)
		}
	}
}

type fakeStore struct {
	created users.NewShare
	err     error
}

func (f *fakeStore) CreateShare(_ context.Context, ns users.NewShare) (users.Share, string, error) {
	f.created = ns
	return users.Share{ID: "sh-1", Owner: ns.Owner, Box: ns.Box, Users: []users.ShareUser{{ID: "u-bob", Name: "bob"}}, Write: ns.Write}, "cbs_x", f.err
}

func (f *fakeStore) Shares(string) []users.Share { return nil }

func (f *fakeStore) SetShareWrite(_ context.Context, _, id string, write bool) (users.Share, error) {
	return users.Share{ID: id, Write: write}, f.err
}

func (f *fakeStore) RegenerateShare(_ context.Context, _, id string) (users.Share, string, error) {
	return users.Share{ID: id}, "cbs_y", f.err
}

func (f *fakeStore) RevokeShare(context.Context, string, string) error { return f.err }
