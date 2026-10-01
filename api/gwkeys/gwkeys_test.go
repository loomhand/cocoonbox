package gwkeys

import (
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
	abort      = -1
)

var keyFormat = regexp.MustCompile(`^cb-[A-Za-z0-9]{45}$`)

func TestAddCreatesThenReuses(t *testing.T) {
	gw, c := newFakeGW(t)
	key, err := c.Add(t.Context(), machine)
	if err != nil || !keyFormat.MatchString(key) {
		t.Fatalf("Add = %q, %v", key, err)
	}
	got := gw.key(key)
	want := map[string]any{"ak": key, "product": "cocoonbox", "tenant": "cocoonbox-boxes", "owner": machine, "qps": 4.0, "daily_token_quota": 5e6, "tokens_per_minute": 2e5}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("created %v, want %v", got, want)
	}
	again, err := c.Add(t.Context(), machine)
	if err != nil || again != key {
		t.Errorf("second Add = %v, want the same key", err)
	}
	other, err := c.Add(t.Context(), rebased)
	if err != nil || other == key {
		t.Errorf("Add for another machine = %v, want a different key", err)
	}
	if gw.posts != 2 {
		t.Errorf("posts = %d, want 2", gw.posts)
	}
}

func TestAddKeepsOneKeyPerMachine(t *testing.T) {
	gw, c := newFakeGW(t)
	gw.put("cb-k1", machine)
	gw.put("cb-k2", machine)
	key, err := c.Add(t.Context(), machine)
	if err != nil || key != "cb-k1" {
		t.Fatalf("Add = %q, %v, want cb-k1", key, err)
	}
	if gw.key("cb-k2") != nil || gw.posts != 0 {
		t.Errorf("extra key kept or a new one created: %v, posts %d", gw.key("cb-k2"), gw.posts)
	}
}

func TestRevokeDeletesOnlyThatMachine(t *testing.T) {
	gw, c := newFakeGW(t)
	gw.put("cb-old", machine)
	gw.put("cb-new", rebased)
	gw.put("cb-other-tenant", machine)
	gw.keys["cb-other-tenant"]["tenant"] = "cocoonbox"
	if err := c.Revoke(t.Context(), machine); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if gw.key("cb-old") != nil || gw.key("cb-new") == nil || gw.key("cb-other-tenant") == nil {
		t.Errorf("keys after Revoke = %v", gw.order)
	}
	if err := c.Revoke(t.Context(), machine); err != nil {
		t.Errorf("Revoke of an absent key: %v", err)
	}
	gw.put("cb-gone", machine)
	gw.vanish = "cb-gone"
	if err := c.Revoke(t.Context(), machine); err != nil {
		t.Errorf("Revoke when DELETE answers 404: %v", err)
	}
}

func TestListPagesThroughTheTenant(t *testing.T) {
	gw, c := newFakeGW(t)
	for i := range pageSize {
		gw.put("cb-filler"+strconv.Itoa(i), "cb-000000000000")
	}
	gw.put("cb-last", machine)
	if err := c.Revoke(t.Context(), machine); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if gw.key("cb-last") != nil || gw.lists != 2 {
		t.Errorf("key on the second page kept: lists %d", gw.lists)
	}
}

func TestErrorsNeverCarryTheKey(t *testing.T) {
	gw, c := newFakeGW(t)
	gw.put("cb-secret", machine)
	for _, fail := range []int{http.StatusInternalServerError, abort} {
		gw.fail = fail
		if err := c.Revoke(t.Context(), machine); err == nil || strings.Contains(err.Error(), "cb-secret") {
			t.Errorf("Revoke with DELETE failing %d: %v", fail, err)
		}
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

func TestNewKeyFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		k := newKey()
		if !keyFormat.MatchString(k) || seen[k] {
			t.Fatalf("newKey = %q", k)
		}
		seen[k] = true
	}
}

type fakeGW struct {
	srv *httptest.Server

	mu     sync.Mutex
	keys   map[string]map[string]any
	order  []string
	posts  int
	lists  int
	fail   int
	vanish string
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
	g.keys[ak] = map[string]any{"ak": ak, "tenant": "cocoonbox-boxes", "owner": owner}
	g.order = append(g.order, ak)
}

func (g *fakeGW) key(ak string) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.keys[ak]
}

func (g *fakeGW) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+adminToken {
		writeError(w, http.StatusUnauthorized, "invalid admin token")
		return
	}
	ak, hasAK := strings.CutPrefix(r.URL.Path, "/admin/keys/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/admin/keys":
		g.lists++
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var page []map[string]any
		for _, k := range g.order {
			if g.keys[k]["tenant"] == r.URL.Query().Get("tenant") {
				page = append(page, g.keys[k])
			}
		}
		page = page[min(offset, len(page)):min(offset+limit, len(page))]
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(page), "offset": offset, "keys": page})
	case r.Method == http.MethodPost && r.URL.Path == "/admin/keys":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.posts++
		g.keys[body["ak"].(string)] = body
		g.order = append(g.order, body["ak"].(string))
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodDelete && hasAK:
		if g.fail == abort {
			panic(http.ErrAbortHandler)
		}
		if g.fail != 0 {
			writeError(w, g.fail, "store error on key "+ak)
			return
		}
		if g.keys[ak] == nil || ak == g.vanish {
			writeError(w, http.StatusNotFound, "key "+ak+" not found")
			return
		}
		delete(g.keys, ak)
		g.order = slices.DeleteFunc(g.order, func(k string) bool { return k == ak })
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}})
}
