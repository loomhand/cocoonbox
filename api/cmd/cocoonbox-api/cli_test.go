package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/store"
	"github.com/loomhand/cocoonbox/api/users"
)

func TestImportStateKeyTakesTheBoxKeyOrDrawsOne(t *testing.T) {
	dir := t.TempDir()
	boxFile := filepath.Join(dir, "cb-9f86d081884c.json")
	secret := bytes.Repeat([]byte{9}, keyring.KeySize)
	if err := os.WriteFile(boxFile, []byte(`{"id":"file:cb-9f86d081884c:1","secret":"CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQk="}`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	keys := &fakeStateKeys{held: map[string]keyring.Key{}}
	if err := importStateKey(t.Context(), keys, "test", boxFile); err != nil {
		t.Fatalf("import box key: %v", err)
	}
	if got := keys.held["test"]; got.ID != "file:cb-9f86d081884c:1" || !bytes.Equal(got.Secret, secret) {
		t.Errorf("imported key %q", got.ID)
	}
	missing := filepath.Join(dir, "cb-ef260e9aa3c6.json")
	for range 2 {
		if err := importStateKey(t.Context(), keys, "dev", missing); err != nil {
			t.Fatalf("import without a file: %v", err)
		}
	}
	if keys.sets != 2 || len(keys.held["dev"].Secret) != keyring.KeySize {
		t.Errorf("%d keys set, dev holds %d bytes; want one new key for dev", keys.sets, len(keys.held["dev"].Secret))
	}
}

func TestOperatorSocketActsAsTheNamedUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	boxes := &fakeBoxes{}
	if err := serveOperator(t.Context(), path, boxes); err != nil {
		t.Fatalf("serveOperator: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket = %v, %v, want mode 0600", info, err)
	}
	conn, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := controlv1.NewBoxesClient(conn)
	if _, err := client.Rebase(t.Context(), &controlv1.RebaseRequest{BoxId: "cb-1"}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Rebase without an operator user = %v, want Unauthenticated", err)
	}
	ctx := metadata.AppendToOutgoingContext(t.Context(), auth.HeaderOperatorUser, "u-1")
	op, err := client.Rebase(ctx, &controlv1.RebaseRequest{BoxId: "cb-1", Image: "cbcore:v1"})
	if err != nil || boxes.user != "u-1" || boxes.image != "cbcore:v1" {
		t.Fatalf("Rebase = %v, %v as %q to %q, want it as u-1 to cbcore:v1", op, err, boxes.user, boxes.image)
	}
	var out bytes.Buffer
	if err := followOperation(ctx, client, op.GetId(), &out); err != nil || !strings.HasSuffix(out.String(), "PHASE_DONE BOX_STATE_RUNNING now on cbcore:v1\n") {
		t.Errorf("follow = %v, printed %q", err, out.String())
	}
	boxes.fail = true
	if err := followOperation(ctx, client, op.GetId(), &out); err == nil || !strings.Contains(err.Error(), "save failed") {
		t.Errorf("follow of a failed operation = %v, want the failure", err)
	}
}

func TestDeleteUserRevokesConnectionsBeforeTheRow(t *testing.T) {
	st := &memStore{objects: map[string]bool{"u-1/boxes/cb-1/head.json": true, "u-2/boxes/cb-2/head.json": true}}
	var calls []string
	revoker := &fakeConnections{calls: &calls}
	accounts := &fakeAccounts{calls: &calls}
	removed, err := deleteUser(t.Context(), st, revoker, accounts, users.User{ID: "u-1", Name: "alice"})
	if err != nil || removed != 1 {
		t.Fatalf("deleteUser = %d, %v", removed, err)
	}
	if strings.Join(calls, " ") != "revoke u-1 delete u-1" {
		t.Errorf("calls = %v, want the revoke before the delete", calls)
	}
	if st.objects["u-1/boxes/cb-1/head.json"] || !st.objects["u-2/boxes/cb-2/head.json"] {
		t.Errorf("objects left = %v", st.objects)
	}

	calls = nil
	revoker.err = errors.New("vendor down")
	if _, err := deleteUser(t.Context(), st, revoker, accounts, users.User{ID: "u-2", Name: "bob"}); err == nil || strings.Join(calls, " ") != "revoke u-2" {
		t.Errorf("deleteUser after a failed revoke = %v, calls %v; want an error and no delete", err, calls)
	}

	calls = nil
	if _, err := deleteUser(t.Context(), st, nil, accounts, users.User{ID: "u-3", Name: "carol"}); err != nil || strings.Join(calls, " ") != "delete u-3" {
		t.Errorf("deleteUser without Connect = %v, calls %v", err, calls)
	}
}

type fakeBoxes struct {
	controlv1.UnimplementedBoxesServer

	user, image string
	fail        bool
}

func (f *fakeBoxes) Rebase(ctx context.Context, req *controlv1.RebaseRequest) (*controlv1.Operation, error) {
	f.user, f.image = auth.User(ctx), req.GetImage()
	return &controlv1.Operation{Id: "op-1", BoxId: req.GetBoxId()}, nil
}

func (f *fakeBoxes) WatchOperation(req *controlv1.WatchOperationRequest, stream grpc.ServerStreamingServer[controlv1.OperationEvent]) error {
	if err := stream.Send(&controlv1.OperationEvent{OperationId: req.GetOperationId(), Phase: controlv1.Phase_PHASE_SAVING, Detail: "saving"}); err != nil {
		return err
	}
	last := &controlv1.OperationEvent{Phase: controlv1.Phase_PHASE_DONE, BoxState: controlv1.BoxState_BOX_STATE_RUNNING, Detail: "now on " + f.image}
	if f.fail {
		last = &controlv1.OperationEvent{Phase: controlv1.Phase_PHASE_FAILED, BoxState: controlv1.BoxState_BOX_STATE_RUNNING, Detail: "save failed"}
	}
	return stream.Send(last)
}

type fakeStateKeys struct {
	held map[string]keyring.Key
	sets int
}

func (f *fakeStateKeys) StateKey(_ context.Context, user string) (keyring.Key, error) {
	k, ok := f.held[user]
	if !ok {
		return keyring.Key{}, errors.New("no state key")
	}
	return k, nil
}

func (f *fakeStateKeys) SetStateKey(_ context.Context, user string, k keyring.Key) error {
	f.sets++
	f.held[user] = k
	return nil
}

type fakeConnections struct {
	calls *[]string
	err   error
}

func (f *fakeConnections) RevokeAll(_ context.Context, user string) error {
	*f.calls = append(*f.calls, "revoke "+user)
	return f.err
}

type fakeAccounts struct {
	calls *[]string
}

func (f *fakeAccounts) Delete(_ context.Context, id string) error {
	*f.calls = append(*f.calls, "delete "+id)
	return nil
}

type memStore struct {
	objects map[string]bool
}

func (m *memStore) PresignPut(context.Context, string) (store.Presigned, error) {
	return store.Presigned{}, errors.ErrUnsupported
}

func (m *memStore) PresignGet(context.Context, string) (store.Presigned, error) {
	return store.Presigned{}, errors.ErrUnsupported
}

func (m *memStore) Get(context.Context, string) ([]byte, error) { return nil, store.ErrNotFound }

func (m *memStore) Put(_ context.Context, key string, _ []byte) error {
	m.objects[key] = true
	return nil
}

func (m *memStore) List(_ context.Context, prefix string) ([]store.Object, error) {
	var out []store.Object
	for key, ok := range m.objects {
		if ok && strings.HasPrefix(key, prefix) {
			out = append(out, store.Object{Key: key})
		}
	}
	return out, nil
}

func (m *memStore) Delete(_ context.Context, keys []string) error {
	for _, key := range keys {
		delete(m.objects, key)
	}
	return nil
}
