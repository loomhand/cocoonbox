package connect

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/connect/connecttest"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
)

const linkRecords = "LINK_ACCESS_TOKEN=cbc_link;api.link.com;Authorization:%20Bearer%20{} LINK_NO_REFRESH=1;;"

func TestLinkGrantInjectsRefreshesAndDisconnects(t *testing.T) {
	env := newEnv(t)
	link := newLink(t, env.svc)
	var hooked []string
	env.svc.OnInjection(func(_ context.Context, user string) { hooked = append(hooked, user) })

	started, err := env.svc.Start(t.Context(), StartRequest{User: alice, SessionSHA256: aliceSess, Connector: linkID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	q := mustQuery(t, started.URL)
	if !strings.HasPrefix(started.URL, link.URL+"/auth?") || q.Get("key") != link.Key || q.Get("client_id") != link.ClientID ||
		q.Get("scope") != "payment_methods.agentic userinfo:read" || q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != callbackURL ||
		q.Has("client_secret") || q.Has("resource") {
		t.Fatalf("authorize url = %s", started.URL)
	}
	code, state, err := link.Consent(started.URL)
	if err != nil || code == "" {
		t.Fatalf("consent: %q %v", code, err)
	}
	if got, err := env.svc.Complete(t.Context(), started.Nonce, state, code, ""); err != nil || got != linkID {
		t.Fatalf("Complete = %q, %v", got, err)
	}
	g, err := env.svc.store.grant(t.Context(), alice, linkID)
	if err != nil || g.account != "buyer@example.com" || len(hooked) != 1 || hooked[0] != alice {
		t.Fatalf("grant = %+v, %v, hook calls %v", g, err, hooked)
	}

	creds, err := env.svc.Credentials(t.Context(), alice)
	sec := linkSecrets(t, env.svc)
	in := creds.Secrets["LINK_API"]
	if err != nil || in == nil || in.Value != "Bearer "+sec.AccessToken || in.Header != "Authorization" || in.Placeholder != "Bearer cbc_link" ||
		len(in.Hosts) != 1 || in.Hosts[0] != "api.link.com" || creds.Flags[guestEnv] != linkRecords {
		t.Fatalf("Credentials = %+v %+v, %v", creds, in, err)
	}
	if creds, _ := env.svc.Credentials(t.Context(), bob); creds.Secrets["LINK_API"] != nil || creds.Flags[guestEnv] != "" {
		t.Errorf("bob's Credentials = %+v", creds)
	}

	env.exec(t, "UPDATE connect_grants SET token_expires_at = now() + interval '1 minute' WHERE connector = 'link'")
	creds, _ = env.svc.Credentials(t.Context(), alice)
	rotated := linkSecrets(t, env.svc)
	if creds.Secrets["LINK_API"].Value != "Bearer "+rotated.AccessToken || rotated.AccessToken == sec.AccessToken || rotated.RefreshToken == sec.RefreshToken {
		t.Errorf("Credentials near expiry = %+v, want the refreshed token", creds.Secrets["LINK_API"])
	}

	current := linkSecrets(t, env.svc)
	revoked, err := env.svc.Remove(t.Context(), alice, linkID)
	if err != nil || !revoked || len(hooked) != 2 {
		t.Fatalf("Remove = %t, %v, hook calls %v", revoked, err, hooked)
	}
	if revokedTokens := link.Revoked(); len(revokedTokens) != 1 || revokedTokens[0] != current.RefreshToken {
		t.Errorf("revoked = %v, want the current refresh token", revokedTokens)
	}
	if creds, _ := env.svc.Credentials(t.Context(), alice); creds.Secrets["LINK_API"] != nil || creds.Flags[guestEnv] != "" {
		t.Errorf("Credentials after Remove = %+v", creds)
	}
}

func TestLinkIsHiddenWithoutItsCredentials(t *testing.T) {
	full := Credentials{ClientID: "id", ClientSecret: "secret", Key: "pk_live_x"}
	for name, creds := range map[string]Credentials{
		"none":      {},
		"no key":    {ClientID: "id", ClientSecret: "secret"},
		"no secret": {ClientID: "id", Key: "pk_live_x"},
		"no id":     {ClientSecret: "secret", Key: "pk_live_x"},
	} {
		svc := &Service{catalog: newCatalog(Options{Link: creds})}
		catalog, _ := svc.Catalog(t.Context(), &controlv1.CatalogRequest{})
		for _, e := range catalog.GetEntries() {
			if e.GetId() == linkID {
				t.Errorf("%s: catalog offers %v", name, e)
			}
		}
		if _, err := svc.Start(t.Context(), StartRequest{User: alice, SessionSHA256: aliceSess, Connector: linkID}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s: Start = %v", name, err)
		}
	}
	svc := &Service{catalog: newCatalog(Options{Link: full})}
	catalog, _ := svc.Catalog(t.Context(), &controlv1.CatalogRequest{})
	var found *controlv1.CatalogEntry
	for _, e := range catalog.GetEntries() {
		if e.GetId() == linkID {
			found = e
		}
	}
	if found == nil || !found.GetAvailable() || !found.GetGrantOnly() || !found.GetCheckable() {
		t.Errorf("link = %v", found)
	}
}

func newLink(t *testing.T, svc *Service) *connecttest.Link {
	t.Helper()
	l := connecttest.NewLink()
	t.Cleanup(l.Close)
	e := svc.entry(linkID)
	e.client = client{ID: l.ClientID, Secret: l.ClientSecret, Key: l.Key, TokenURL: l.URL + "/auth/token", RevokeURL: l.URL + "/auth/revoke"}
	e.authorizeURL, e.params, e.userInfoURL = l.URL+"/auth", url.Values{"key": {l.Key}}, l.URL+"/userinfo"
	return l
}

func linkSecrets(t *testing.T, svc *Service) *secrets {
	t.Helper()
	g, err := svc.store.grant(t.Context(), alice, linkID)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sec, err := svc.secrets(t.Context(), g)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return sec
}
