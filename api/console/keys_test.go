package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CMGS/cocoonbox/api/users"
	"github.com/CMGS/cocoonbox/api/vault"
)

func TestTheKeysPageAddsListsRotatesAndDeletesWithoutShowingAValue(t *testing.T) {
	env := newEnv(t)
	keys, err := vault.Open(t.Context(), vault.Options{DSN: env.dsn, Keys: env.users})
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	t.Cleanup(keys.Close)
	env.srv.EnableKeys(keys)
	if _, err := env.users.Create(t.Context(), users.NewUser{Name: "qa1", Password: password}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	qa1 := env.login(t, "qa1")
	const value = "synthetic-console-0123456789"

	resp := env.do(t, http.MethodPost, "api/keys", qa1, `{"name":"ATTIO_API_KEY","hosts":["api.attio.com"],"value":"`+value+`"}`)
	if resp.Code != http.StatusOK || strings.Contains(resp.Body.String(), value) || !strings.Contains(resp.Body.String(), `"hint":"6789"`) {
		t.Errorf("add = %d %s", resp.Code, resp.Body)
	}
	if code := env.do(t, http.MethodPost, "api/keys", qa1, `{"name":"ATTIO_API_KEY","hosts":["api.attio.com"],"value":"`+value+`"}`).Code; code != http.StatusConflict {
		t.Errorf("second add = %d, want 409", code)
	}
	if resp := env.do(t, http.MethodPost, "api/keys", qa1, `{"name":"X","hosts":["*.vercel.app"],"value":"v"}`); resp.Code != http.StatusBadRequest {
		t.Errorf("add on *.vercel.app = %d %s", resp.Code, resp.Body)
	}
	list := env.do(t, http.MethodGet, "api/keys", qa1, "").Body.String()
	if !strings.Contains(list, `"name":"ATTIO_API_KEY"`) || !strings.Contains(list, `"template":"Bearer {}"`) || strings.Contains(list, value) {
		t.Errorf("list = %s", list)
	}
	if resp := env.do(t, http.MethodPut, "api/keys/ATTIO_API_KEY", qa1, `{"value":"synthetic-rotated-abcdefgh"}`); resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"hint":"efgh"`) {
		t.Errorf("rotate = %d %s", resp.Code, resp.Body)
	}
	if code := env.do(t, http.MethodDelete, "api/keys/ATTIO_API_KEY", qa1, "").Code; code != http.StatusNoContent {
		t.Errorf("delete = %d", code)
	}
	if code := env.do(t, http.MethodDelete, "api/keys/ATTIO_API_KEY", qa1, "").Code; code != http.StatusNotFound {
		t.Errorf("second delete = %d", code)
	}
	if code := env.do(t, http.MethodGet, "api/keys", "", "").Code; code != http.StatusUnauthorized {
		t.Errorf("list signed out = %d", code)
	}
	page := httptest.NewRecorder()
	env.srv.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/cb/keys", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "keys.js") {
		t.Errorf("page = %d", page.Code)
	}
}
