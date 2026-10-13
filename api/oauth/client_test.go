package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckRedirect(t *testing.T) {
	tests := []struct {
		uri string
		ok  bool
	}{
		{"https://claude.ai/api/mcp/auth_callback", true},
		{"http://localhost/callback", true},
		{"http://127.0.0.1:33418/callback", true},
		{"http://[::1]:8080/cb", true},
		{"com.example.app:/oauth", true},
		{"http://evil.example/callback", false},
		{"https://example.com/cb#frag", false},
		{"javascript:alert(1)", false},
		{"cursor://anysphere/oauth", false},
		{"https://user@example.com/cb", false},
	}
	for _, tt := range tests {
		if err := checkRedirect(tt.uri); (err == nil) != tt.ok {
			t.Errorf("checkRedirect(%q) = %v, want ok %t", tt.uri, err, tt.ok)
		}
	}
}

func TestSameRedirectIgnoresOnlyTheLoopbackPort(t *testing.T) {
	tests := []struct {
		registered, uri string
		want            bool
	}{
		{"http://localhost/callback", "http://localhost:51234/callback", true},
		{"http://127.0.0.1/callback", "http://127.0.0.1:1/callback", true},
		{"http://localhost/callback", "http://127.0.0.1:51234/callback", false},
		{"http://localhost/callback", "http://localhost:51234/other", false},
		{"https://app.example/cb", "https://app.example:444/cb", false},
		{"https://app.example/cb", "https://app.example/cb", true},
	}
	for _, tt := range tests {
		if got := sameRedirect(tt.registered, tt.uri); got != tt.want {
			t.Errorf("sameRedirect(%q, %q) = %t, want %t", tt.registered, tt.uri, got, tt.want)
		}
	}
}

func TestRegisteredClientCarriesItsMetadataAndRefusesTampering(t *testing.T) {
	s := &Service{key: []byte("k")}
	id, err := s.register("Codex", []string{"http://127.0.0.1/callback"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	c, err := s.client(t.Context(), id)
	if err != nil || c.name != "Codex" || c.host != "" || !c.allows("http://127.0.0.1:4000/callback") {
		t.Fatalf("client = %+v, %v", c, err)
	}
	payload, mac, _ := strings.Cut(strings.TrimPrefix(id, dcrPrefix), ".")
	forged, _ := (&Service{key: []byte("other")}).register("Codex", []string{"https://evil.example/cb"})
	forgedPayload, _, _ := strings.Cut(strings.TrimPrefix(forged, dcrPrefix), ".")
	for _, bad := range []string{dcrPrefix + forgedPayload + "." + mac, dcrPrefix + payload + ".AAAA", dcrPrefix + payload, "plain-id"} {
		if _, err := s.client(t.Context(), bad); err == nil {
			t.Errorf("client(%q) admitted a forged id", bad)
		}
	}
}

func TestMetadataDocumentClient(t *testing.T) {
	var body, contentType string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	id := srv.URL + "/client.json"
	tests := []struct {
		name, body, contentType string
		ok                      bool
	}{
		{"valid", `{"client_id":"` + id + `","client_name":"Claude Code","redirect_uris":["http://localhost/callback"],"token_endpoint_auth_method":"none","client_uri":"https://x.example","grant_types":["authorization_code"]}`, "application/json", true},
		{"another client id", `{"client_id":"https://evil.example/c.json","client_name":"X","redirect_uris":["http://localhost/callback"]}`, "application/json", false},
		{"a confidential client", `{"client_id":"` + id + `","client_name":"X","redirect_uris":["http://localhost/callback"],"token_endpoint_auth_method":"private_key_jwt"}`, "application/json", false},
		{"no redirect", `{"client_id":"` + id + `","client_name":"X","redirect_uris":[]}`, "application/json", false},
		{"html", `<html></html>`, "text/html", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType = tt.body, tt.contentType
			s := &Service{fetch: srv.Client(), documents: &documents{entries: map[string]cachedDocument{}}}
			c, err := s.client(t.Context(), id)
			if (err == nil) != tt.ok {
				t.Fatalf("client = %+v, %v, want ok %t", c, err, tt.ok)
			}
			if tt.ok && (c.name != "Claude Code" || c.host != "127.0.0.1" || !c.allows("http://localhost:7777/callback")) {
				t.Errorf("client = %+v", c)
			}
		})
	}
}
