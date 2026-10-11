package connect

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/connect/connecttest"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

const linkRecords = "LINK_ACCESS_TOKEN=cbc_link;api.link.com;Authorization:%20Bearer%20{} LINK_NO_REFRESH=1;;"

func TestLinkGrantInjectsRefreshesPollsAndDisconnects(t *testing.T) {
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
	if got, err := env.svc.Complete(t.Context(), started.Nonce, state, code); err != nil || got != linkID {
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

	link.SetSpends(
		map[string]any{
			"id": "lsrq_1", "status": "pending_approval", "amount": 2599, "currency": "usd", "merchant_name": "Shop", "merchant_url": "https://shop.example",
			"approval_url": "https://app.link.com/activity/approve/lsrq_1", "created_at": "2026-10-11T10:00:00Z", "expires_at": 1791800000, "context": "a book",
		},
		map[string]any{"id": "lsrq_2", "status": "approved", "amount": 100, "created_at": "2026-10-11T09:00:00Z"},
		map[string]any{"id": "lsrq_3", "status": "requires_action", "amount": 700, "created_at": "2026-10-11T09:30:00Z"},
	)
	link.ExpireAccess()
	env.svc.pollSpends(t.Context())
	ctx := auth.WithUser(t.Context(), alice)
	got, err := env.svc.SpendRequests(ctx, &controlv1.SpendRequestsRequest{})
	if err != nil || len(got.GetRequests()) != 2 || got.GetPolledAtMs() == 0 {
		t.Fatalf("SpendRequests = %v, %v", got, err)
	}
	first := got.GetRequests()[0]
	if first.GetId() != "lsrq_1" || first.GetAmount() != 2599 || first.GetCurrency() != "usd" || first.GetMerchantName() != "Shop" ||
		first.GetMerchantUrl() != "https://shop.example" || first.GetStatus() != "pending_approval" || first.GetApprovalUrl() != "https://app.link.com/activity/approve/lsrq_1" ||
		first.GetCreatedAtMs() != time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC).UnixMilli() || first.GetExpiresAtMs() != 1791800000000 {
		t.Errorf("first = %v", first)
	}
	if got.GetRequests()[1].GetStatus() != "requires_action" {
		t.Errorf("second = %v", got.GetRequests()[1])
	}
	if refreshes, polls, _ := link.Snapshot(); refreshes != 2 || polls != 2 {
		t.Errorf("refreshes, polls = %d, %d, want 2, 2 after a 401", refreshes, polls)
	}
	if got, err := env.svc.SpendRequests(auth.WithUser(t.Context(), bob), &controlv1.SpendRequestsRequest{}); err != nil || len(got.GetRequests()) != 0 || got.GetPolledAtMs() != 0 {
		t.Errorf("bob's SpendRequests = %v, %v", got, err)
	}

	link.FailSpends(true)
	env.svc.pollSpends(t.Context())
	env.svc.pollSpends(t.Context())
	if _, polls, _ := link.Snapshot(); polls != 3 {
		t.Errorf("polls = %d, want 3: a failed user waits out its backoff", polls)
	}
	if again, _ := env.svc.SpendRequests(ctx, &controlv1.SpendRequestsRequest{}); len(again.GetRequests()) != 2 || again.GetPolledAtMs() != got.GetPolledAtMs() {
		t.Errorf("SpendRequests after a failed read = %v, want the last good one", again)
	}

	current := linkSecrets(t, env.svc)
	revoked, err := env.svc.Disconnect(ctx, &controlv1.ConnectorRef{ConnectorId: linkID})
	if err != nil || !revoked.GetRevoked() || len(hooked) != 2 {
		t.Fatalf("Disconnect = %v, %v, hook calls %v", revoked, err, hooked)
	}
	if _, _, revokedTokens := link.Snapshot(); len(revokedTokens) != 1 || revokedTokens[0] != current.RefreshToken {
		t.Errorf("revoked = %v, want the current refresh token", revokedTokens)
	}
	if creds, _ := env.svc.Credentials(t.Context(), alice); creds.Secrets["LINK_API"] != nil || creds.Flags[guestEnv] != "" {
		t.Errorf("Credentials after Disconnect = %+v", creds)
	}
	if got, _ := env.svc.SpendRequests(ctx, &controlv1.SpendRequestsRequest{}); len(got.GetRequests()) != 0 || got.GetPolledAtMs() != 0 {
		t.Errorf("SpendRequests after Disconnect = %v", got)
	}
	link.FailSpends(false)
	env.svc.pollSpends(t.Context())
	if _, polls, _ := link.Snapshot(); polls != 3 || len(env.svc.spends) != 0 {
		t.Errorf("polls = %d, spends = %v after Disconnect", polls, env.svc.spends)
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
	if found == nil || !found.GetAvailable() || !found.GetGrantOnly() || !found.GetCheckable() || found.GetAuth() != string(authOAuth) || found.GetMcpUrl() != "" {
		t.Errorf("link = %v", found)
	}
}

func TestThePollerDoesNothingWithoutLinkGrants(t *testing.T) {
	env := newEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	env.svc.RunSpendRequests(ctx)
	if time.Since(start) > 100*time.Millisecond {
		t.Errorf("RunSpendRequests ran %s without Link set up", time.Since(start))
	}

	link := newLink(t, env.svc)
	env.connect(t)
	env.exec(t, "INSERT INTO connect_grants (id, user_id, connector, kind, mcp_url, status, sealed) VALUES ('cg_lost', 'u-bob', 'link', 'link', '', 'needs_reconnect', '\\x00')")
	env.svc.pollSpends(t.Context())
	if _, polls, _ := link.Snapshot(); polls != 0 || len(env.svc.spends) != 0 {
		t.Errorf("polls = %d, spends = %v without an active Link grant", polls, env.svc.spends)
	}
}

// newLink points the catalog's Link entry and the Link API at a mock Link.
func newLink(t *testing.T, svc *Service) *connecttest.Link {
	t.Helper()
	l := connecttest.NewLink()
	t.Cleanup(l.Close)
	e := svc.entry(linkID)
	e.client = client{ID: l.ClientID, Secret: l.ClientSecret, Key: l.Key, TokenURL: l.URL + "/auth/token", RevokeURL: l.URL + "/auth/revoke"}
	e.authorizeURL, e.params, e.userInfoURL = l.URL+"/auth", url.Values{"key": {l.Key}}, l.URL+"/userinfo"
	svc.linkAPI = l.URL
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
