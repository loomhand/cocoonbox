package flows

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/keyring"
	"github.com/loomhand/cocoonbox/api/store"
)

func TestPutKeepsTheLastWriteAndSealsItAtRest(t *testing.T) {
	st, lib := newLibrary()
	ctx := t.Context()
	first, err := lib.Put(ctx, "alice", "mousepad-save.json", []byte(`{"name":"mousepad-save","typed":"secret"}`), Origin{BoxID: "cb-1", Image: "cbcore:v17"})
	if err != nil || first.Rev != 1 || first.BoxID != "cb-1" || first.Image != "cbcore:v17" {
		t.Fatalf("first Put = %+v, %v", first, err)
	}
	same, err := lib.Put(ctx, "alice", "mousepad-save.json", []byte(`{"name":"mousepad-save","typed":"secret"}`), Origin{BoxID: "cb-2"})
	if err != nil || same != first {
		t.Errorf("same content Put = %+v, %v, want %+v", same, err, first)
	}
	second, err := lib.Put(ctx, "alice", "mousepad-save.json", []byte(`{"name":"mousepad-save","v":2}`), Origin{BoxID: "cb-2"})
	if err != nil || second.Rev != 2 || second.BoxID != "cb-2" {
		t.Fatalf("second Put = %+v, %v", second, err)
	}
	e, content, err := lib.Get(ctx, "alice", "mousepad-save.json")
	if err != nil || e != second || string(content) != `{"name":"mousepad-save","v":2}` {
		t.Errorf("Get = %+v %q %v", e, content, err)
	}
	keys := st.keys()
	if len(keys) != 2 || !slices.Contains(keys, "alice/flows/index") {
		t.Errorf("store keys %v, want the index and one object", keys)
	}
	for k, v := range st.snapshot() {
		if bytes.Contains(v, []byte("mousepad")) || bytes.Contains(v, []byte("secret")) {
			t.Errorf("%s holds plaintext", k)
		}
	}
}

func TestLibrariesStayApart(t *testing.T) {
	_, lib := newLibrary()
	ctx := t.Context()
	if _, err := lib.Put(ctx, "alice", "a.mts", []byte("export function main() {}"), Origin{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lib.Get(ctx, "bob", "a.mts"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob's Get = %v", err)
	}
	if rev, entries, err := lib.List(ctx, "bob"); err != nil || rev != 0 || len(entries) != 0 {
		t.Errorf("bob's List = %d %v %v", rev, entries, err)
	}
}

func TestPutRefusesBadNamesAndContent(t *testing.T) {
	_, lib := newLibrary()
	for name, content := range map[string][]byte{
		"../x.json":     []byte("{}"),
		".pending.json": []byte("{}"),
		"INDEX":         []byte("x"),
		"a.txt":         []byte("x"),
		"sub/a.json":    []byte("{}"),
		"bin.json":      {0xff, 0xfe},
	} {
		if _, err := lib.Put(t.Context(), "alice", name, content, Origin{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Put %q = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := lib.Put(t.Context(), "alice", "big.json", bytes.Repeat([]byte("a"), MaxContent+1), Origin{}); !errors.Is(err, ErrFull) {
		t.Errorf("oversized Put = %v", err)
	}
}

func TestPutStopsAtTheLibraryLimits(t *testing.T) {
	_, lib := newLibrary()
	ctx := t.Context()
	big := bytes.Repeat([]byte("a"), MaxContent)
	for i := range MaxBytes / MaxContent {
		if _, err := lib.Put(ctx, "alice", "f"+string(rune('a'+i%26))+strings.Repeat("x", i/26)+".json", big, Origin{}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	if _, err := lib.Put(ctx, "alice", "over.json", []byte("{}"), Origin{}); !errors.Is(err, ErrFull) {
		t.Errorf("Put past MaxBytes = %v", err)
	}
	if _, err := lib.Put(ctx, "alice", "fa.json", []byte("{}"), Origin{}); err != nil {
		t.Errorf("replacing a flow within the limit: %v", err)
	}
}

func TestDeleteDropsTheFlowAndItsObject(t *testing.T) {
	st, lib := newLibrary()
	ctx := t.Context()
	for _, name := range []string{"a.json", "b.mts"} {
		if _, err := lib.Put(ctx, "alice", name, []byte(name), Origin{}); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := lib.Delete(ctx, "alice", []string{"a.json", "missing.json"})
	if err != nil || !slices.Equal(deleted, []string{"a.json"}) {
		t.Fatalf("Delete = %v, %v", deleted, err)
	}
	rev, entries, err := lib.List(ctx, "alice")
	if err != nil || rev != 3 || len(entries) != 1 || entries[0].Name != "b.mts" {
		t.Errorf("List = %d %+v %v", rev, entries, err)
	}
	if len(st.keys()) != 2 {
		t.Errorf("store keys %v, want the index and b's object", st.keys())
	}
	if deleted, err := lib.Delete(ctx, "alice", []string{"missing.json"}); err != nil || deleted != nil {
		t.Errorf("Delete of nothing = %v, %v", deleted, err)
	}
}

func TestDoorServesTheMachineOwnersLibrary(t *testing.T) {
	_, lib := newLibrary()
	door := httptest.NewServer(NewDoor(lib, doorKeys, func(_ context.Context, owner, machine string) Origin {
		return Origin{BoxID: strings.TrimSuffix(machine, "-r1"), Image: "cbcore:v17"}
	}))
	defer door.Close()

	if code, _ := call(t, door.URL, "", http.MethodGet, "/flows", ""); code != http.StatusUnauthorized {
		t.Errorf("no key: %d", code)
	}
	if code, body := call(t, door.URL, "alice-key", http.MethodPut, "/flows/save.json", `{"steps":[]}`); code != http.StatusOK || !strings.Contains(body, `"box_id":"cb-a"`) {
		t.Fatalf("PUT = %d %s", code, body)
	}
	code, body := call(t, door.URL, "alice-key", http.MethodGet, "/flows", "")
	var listed struct {
		Rev   uint64  `json:"rev"`
		Flows []Entry `json:"flows"`
	}
	if err := json.Unmarshal([]byte(body), &listed); code != http.StatusOK || err != nil || listed.Rev != 1 || listed.Flows[0].Image != "cbcore:v17" {
		t.Errorf("GET list = %d %s %v", code, body, err)
	}
	if code, body := call(t, door.URL, "alice-key", http.MethodGet, "/flows/save.json", ""); code != http.StatusOK || body != `{"steps":[]}` {
		t.Errorf("GET flow = %d %q", code, body)
	}
	if code, _ := call(t, door.URL, "bob-key", http.MethodGet, "/flows/save.json", ""); code != http.StatusNotFound {
		t.Errorf("bob's GET = %d", code)
	}
	if code, _ := call(t, door.URL, "alice-key", http.MethodPut, "/flows/x.txt", "x"); code != http.StatusBadRequest {
		t.Errorf("bad name = %d", code)
	}
	if code, _ := call(t, door.URL, "alice-key", http.MethodPut, "/flows/big.json", strings.Repeat("a", MaxContent+1)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("big = %d", code)
	}
}

func TestServerExportsAndImportsTheCallersFlowsOnly(t *testing.T) {
	_, lib := newLibrary()
	srv := NewServer(lib)
	alice := auth.WithUser(t.Context(), "alice")
	if _, err := srv.List(t.Context(), &controlv1.ListFlowsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("List without a user: %v", err)
	}
	if _, err := lib.Put(alice, "alice", "save.json", []byte(`{"v":1}`), Origin{BoxID: "cb-a", Image: "cbcore:v17"}); err != nil {
		t.Fatal(err)
	}
	exported, err := srv.Export(alice, &controlv1.ExportFlowsRequest{})
	if err != nil || len(exported.GetFlows()) != 1 || string(exported.GetFlows()[0].GetContent()) != `{"v":1}` {
		t.Fatalf("Export = %v, %v", exported, err)
	}
	if _, err := srv.Export(alice, &controlv1.ExportFlowsRequest{Names: []string{"none.json"}}); status.Code(err) != codes.NotFound {
		t.Errorf("Export of a missing flow: %v", err)
	}
	bob := auth.WithUser(t.Context(), "bob")
	if got, err := srv.Export(bob, &controlv1.ExportFlowsRequest{}); err != nil || len(got.GetFlows()) != 0 {
		t.Errorf("bob's Export = %v, %v", got, err)
	}
	imported, err := srv.Save(bob, &controlv1.SaveFlowsRequest{Flows: exported.GetFlows()})
	if err != nil || imported.GetFlows()[0].GetRev() != 1 || imported.GetFlows()[0].GetImage() != "cbcore:v17" || imported.GetFlows()[0].GetBoxId() != "" {
		t.Fatalf("bob's Save = %v, %v", imported, err)
	}
	bad := &controlv1.SaveFlowsRequest{Flows: []*controlv1.Flow{{Name: "ok.json", Content: []byte("{}")}, {Name: "../x", Content: []byte("{}")}}}
	if _, err := srv.Save(bob, bad); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Save with a bad name: %v", err)
	}
	if listed, _ := srv.List(bob, &controlv1.ListFlowsRequest{}); len(listed.GetFlows()) != 1 {
		t.Errorf("a refused Save wrote %v", listed.GetFlows())
	}
	deleted, err := srv.Delete(bob, &controlv1.DeleteFlowsRequest{Names: []string{"save.json"}})
	if err != nil || !slices.Equal(deleted.GetDeleted(), []string{"save.json"}) {
		t.Errorf("Delete = %v, %v", deleted, err)
	}
	if listed, _ := srv.List(alice, &controlv1.ListFlowsRequest{}); len(listed.GetFlows()) != 1 {
		t.Errorf("bob's Delete reached alice: %v", listed.GetFlows())
	}
}

func newLibrary() (*memStore, *Library) {
	st := &memStore{objects: map[string][]byte{}}
	return st, NewLibrary(st, fixedKeys{})
}

func doorKeys(_ context.Context, header string) (string, string, error) {
	switch header {
	case "Bearer alice-key":
		return "alice", "cb-a-r1", nil
	case "Bearer bob-key":
		return "bob", "cb-b", nil
	}
	return "", "", errors.New("no door key")
}

func call(t *testing.T, base, key, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, strings.TrimSpace(out.String())
}

type fixedKeys struct{}

func (fixedKeys) StateKey(_ context.Context, user string) (keyring.Key, error) {
	return keyring.Key{ID: user, Secret: bytes.Repeat([]byte(user[:1]), keyring.KeySize)}, nil
}

type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *memStore) PresignPut(context.Context, string) (store.Presigned, error) {
	return store.Presigned{}, errors.New("unused")
}

func (m *memStore) PresignGet(context.Context, string) (store.Presigned, error) {
	return store.Presigned{}, errors.New("unused")
}

func (m *memStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.objects[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return v, nil
}

func (m *memStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = slices.Clone(data)
	return nil
}

func (m *memStore) List(_ context.Context, prefix string) ([]store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Object
	for k, v := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, store.Object{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}

func (m *memStore) Delete(_ context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.objects, k)
	}
	return nil
}

func (m *memStore) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.objects))
}

func (m *memStore) snapshot() map[string][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.objects)
}
