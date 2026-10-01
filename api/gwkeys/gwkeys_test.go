package gwkeys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	adminToken = "tenant-admin-token"
	machine    = "cb-ef260e9aa3c6"
	rebased    = "cb-ef260e9aa3c6-r1"
)

var (
	keyFormat = regexp.MustCompile(`^gw-[0-9a-f]{64}$`)
	idFormat  = regexp.MustCompile(`^sha256:[0-9a-f]{32}$`)
)

func TestAddCreatesAGeneratedKey(t *testing.T) {
	gw, c := newFakeGW(t)
	key, err := c.Add(t.Context(), machine)
	if err != nil || !keyFormat.MatchString(key) {
		t.Fatalf("Add = %q, %v", key, err)
	}
	got := gw.key(akID(key))
	want := map[string]any{"ak_id": akID(key), "product": "cocoonbox", "tenant": "cocoonbox-boxes", "owner": machine, "qps": 4.0, "daily_token_quota": 5e6, "tokens_per_minute": 2e5}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("created %v, want %v", got, want)
	}
	other, err := c.Add(t.Context(), rebased)
	if err != nil || other == key || gw.key(akID(key)) == nil {
		t.Errorf("Add for another machine = %v, want a different key and the first kept", err)
	}
}

func TestAddReplacesTheMachinesKeys(t *testing.T) {
	gw, c := newFakeGW(t)
	gw.put("gw-k1", machine)
	gw.put("gw-k2", machine)
	key, err := c.Add(t.Context(), machine)
	if err != nil || key == "gw-k1" || key == "gw-k2" {
		t.Fatalf("Add = %q, %v, want a fresh key", key, err)
	}
	again, err := c.Add(t.Context(), machine)
	if err != nil || again == key {
		t.Fatalf("second Add = %q, %v, want a fresh key", again, err)
	}
	if ids := gw.idsOf(machine); !slices.Equal(ids, []string{akID(again)}) {
		t.Errorf("keys of %s = %v, want only the last one", machine, ids)
	}
}

func TestRevokeDeletesOnlyThatMachine(t *testing.T) {
	gw, c := newFakeGW(t)
	gw.put("gw-old", machine)
	gw.put("gw-new", rebased)
	gw.put("gw-other-tenant", machine)
	gw.keys[akID("gw-other-tenant")]["tenant"] = "cocoonbox"
	if err := c.Revoke(t.Context(), machine); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if gw.key(akID("gw-old")) != nil || gw.key(akID("gw-new")) == nil || gw.key(akID("gw-other-tenant")) == nil {
		t.Errorf("keys after Revoke = %v", gw.order)
	}
	if err := c.Revoke(t.Context(), machine); err != nil {
		t.Errorf("Revoke of an absent key: %v", err)
	}
	gw.put("gw-gone", machine)
	gw.vanish = akID("gw-gone")
	if err := c.Revoke(t.Context(), machine); err != nil {
		t.Errorf("Revoke when DELETE answers 404: %v", err)
	}
}

func TestListFiltersByOwner(t *testing.T) {
	gw, c := newFakeGW(t)
	for i := range pageSize {
		gw.put("gw-filler"+strconv.Itoa(i), "cb-000000000000")
	}
	gw.put("gw-last", machine)
	if err := c.Revoke(t.Context(), machine); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if gw.key(akID("gw-last")) != nil || gw.lists != 1 {
		t.Errorf("key of %s kept or the tenant scanned: lists %d", machine, gw.lists)
	}
}

func TestErrorsNeverCarryTheKey(t *testing.T) {
	gw, c := newFakeGW(t)
	gw.cut = true
	if key, err := c.Add(t.Context(), machine); err == nil || key != "" || strings.Contains(err.Error(), gw.last) {
		t.Errorf("Add with the POST response cut = %q, %v", key, err)
	}
	gw.cut = false
	key, err := c.Add(t.Context(), machine)
	if err != nil || !slices.Equal(gw.idsOf(machine), []string{akID(key)}) {
		t.Errorf("retried Add = %v, keys %v, want the orphan revoked", err, gw.idsOf(machine))
	}
	bad, _ := New(gw.srv.URL, "wrong")
	if _, err := bad.Add(t.Context(), machine); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("Add with a wrong token: %v", err)
	}
}

func TestNewRejectsBadURL(t *testing.T) {
	for _, base := range []string{"", "10.203.0.1:18090", "unix:///run/gw.sock"} {
		if _, err := New(base, adminToken); err == nil {
			t.Errorf("New(%q) succeeded", base)
		}
	}
}

type fakeGW struct {
	srv *httptest.Server

	mu     sync.Mutex
	keys   map[string]map[string]any
	order  []string
	lists  int
	vanish string
	cut    bool
	last   string
}

func newFakeGW(t *testing.T) (*fakeGW, *Client) {
	t.Helper()
	gw := &fakeGW{keys: map[string]map[string]any{}}
	gw.srv = httptest.NewServer(http.HandlerFunc(gw.serve))
	t.Cleanup(gw.srv.Close)
	c, err := New(gw.srv.URL, adminToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return gw, c
}

func (g *fakeGW) put(ak, owner string) {
	id := akID(ak)
	g.keys[id] = map[string]any{"ak_id": id, "tenant": "cocoonbox-boxes", "owner": owner}
	g.order = append(g.order, id)
}

func (g *fakeGW) key(id string) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.keys[id]
}

func (g *fakeGW) idsOf(owner string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var ids []string
	for _, id := range g.order {
		if g.keys[id]["owner"] == owner {
			ids = append(ids, id)
		}
	}
	return ids
}

func (g *fakeGW) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+adminToken {
		writeError(w, http.StatusUnauthorized, "invalid admin token")
		return
	}
	id, hasID := strings.CutPrefix(r.URL.Path, "/admin/keys/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/admin/keys":
		g.lists++
		q := r.URL.Query()
		if q.Get("owner") == "" {
			writeError(w, http.StatusBadRequest, "owner must be non-empty")
			return
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		var page []map[string]any
		for _, k := range g.order {
			if g.keys[k]["tenant"] == q.Get("tenant") && g.keys[k]["owner"] == q.Get("owner") {
				page = append(page, g.keys[k])
			}
		}
		page = page[:min(limit, len(page))]
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(page), "offset": 0, "keys": page})
	case r.Method == http.MethodPost && r.URL.Path == "/admin/keys":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["ak"]; ok {
			writeError(w, http.StatusBadRequest, "the client chose the key")
			return
		}
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		ak := "gw-" + hex.EncodeToString(raw)
		body["ak_id"] = akID(ak)
		g.keys[akID(ak)] = body
		g.order = append(g.order, akID(ak))
		g.last = ak
		w.WriteHeader(http.StatusCreated)
		resp := fmt.Sprintf(`{"ak_id":%q,"status":"created","ak":%q}`, akID(ak), ak)
		if g.cut {
			_, _ = w.Write([]byte(resp[:len(resp)-1]))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write([]byte(resp))
	case r.Method == http.MethodDelete && hasID:
		if !idFormat.MatchString(id) {
			writeError(w, http.StatusBadRequest, "the client deleted by raw key")
			return
		}
		if g.keys[id] == nil || id == g.vanish {
			writeError(w, http.StatusNotFound, "key "+id+" not found")
			return
		}
		delete(g.keys, id)
		g.order = slices.DeleteFunc(g.order, func(k string) bool { return k == id })
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// akID is gw's access_key_id: sha256: plus the first 16 digest bytes in hex.
func akID(ak string) string {
	sum := sha256.Sum256([]byte(ak))
	return "sha256:" + hex.EncodeToString(sum[:16])
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}})
}
