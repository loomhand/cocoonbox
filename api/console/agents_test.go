package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTheSetupPageGivesTheThreeClientsTheOneURL(t *testing.T) {
	srv := New(nil, nil, 0, 1)
	srv.EnableAgents(nil)
	for path, want := range map[string][]string{
		setupPage: {
			"<h2>Claude Code</h2>", "claude mcp add --transport http --scope user cocoonbox", "claude mcp login cocoonbox",
			"<h2>Codex</h2>", "codex mcp add cocoonbox --url", "codex mcp login cocoonbox", "[mcp_servers.cocoonbox]",
			"<h2>Cursor</h2>", `"mcpServers"`, "agent mcp login cocoonbox", `id="cursor-install"`,
		},
		Prefix + "setup.js": {`location.origin + "/cocoonbox.mcp"`, "cursor://anysphere.cursor-deeplink/mcp/install?name=cocoonbox&config="},
		Prefix:              {`href="/cb/setup"`},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		for _, s := range want {
			if !strings.Contains(rec.Body.String(), s) {
				t.Errorf("%s lacks %q", path, s)
			}
		}
		if strings.Contains(rec.Body.String(), "Bearer") {
			t.Errorf("%s carries a token header", path)
		}
	}
}
