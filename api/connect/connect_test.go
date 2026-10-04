package connect

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/connect/connecttest"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/keyring"
)

const (
	alice       = "u-alice"
	bob         = "u-bob"
	aliceSess   = "alice-session-sha"
	publicURL   = "https://cb.example"
	mockName    = "mock"
	createItem  = mockName + "_create_item"
	listItems   = mockName + "_list_items"
	callbackURL = publicURL + CallbackPath
)

var (
	// testKeys gives every user a fixed state key of its own.
	testKeys = keyFunc(func(_ context.Context, user string) (keyring.Key, error) {
		return keyring.Key{ID: "k-" + user, Secret: []byte(strings.Repeat(user[len(user)-1:], keyring.KeySize))}, nil
	})

	modeB = caller{user: alice, door: DoorMCP, name: "mcp:test"}
	modeA = caller{user: alice, door: DoorBox, name: "box:test"}
)

func TestSignInWithMCPAuthorizationBindsTheFlowToItsSession(t *testing.T) {
	env := newEnv(t)
	authorize, id, err := env.svc.Start(t.Context(), StartRequest{User: alice, SessionSHA256: aliceSess, Connector: kindCustom, Name: mockName, URL: env.vendor.MCPURL()})
	if err != nil || id != mockName {
		t.Fatalf("Start = %q, %q, %v", authorize, id, err)
	}
	q := mustQuery(t, authorize)
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("redirect_uri") != callbackURL ||
		q.Get("resource") != env.vendor.MCPURL() || !strings.HasPrefix(q.Get("client_id"), "dcr-") || q.Get("scope") != "items:read items:write" {
		t.Fatalf("authorize query = %v", q)
	}
	code, state, err := env.vendor.Consent(authorize)
	if err != nil || code == "" {
		t.Fatalf("consent: %q %v", code, err)
	}
	if _, err := env.svc.Complete(t.Context(), alice, "another-session", state, code); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Complete from another session: %v", err)
	}
	if _, err := env.svc.Complete(t.Context(), bob, aliceSess, state, code); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Complete as another user: %v", err)
	}
	if got, err := env.svc.Complete(t.Context(), alice, aliceSess, state, code); err != nil || got != mockName {
		t.Fatalf("Complete = %q, %v", got, err)
	}
	if _, err := env.svc.Complete(t.Context(), alice, aliceSess, state, code); status.Code(err) != codes.PermissionDenied {
		t.Errorf("replayed state: %v", err)
	}
	list, err := env.svc.List(auth.WithUser(t.Context(), alice), &controlv1.ListConnectorsRequest{WithTools: true})
	if err != nil || len(list.GetConnectors()) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	c := list.GetConnectors()[0]
	if c.GetStatus() != statusActive || c.GetKind() != kindCustom || len(c.GetTools()) != 2 || c.GetTools()[0].GetEffective() != controlv1.Policy_POLICY_ALLOW ||
		c.GetTools()[1].GetEffective() != controlv1.Policy_POLICY_ASK {
		t.Errorf("connector = %v", c)
	}
	if strings.Contains(c.String(), "at-") || strings.Contains(c.String(), "rt-") {
		t.Errorf("List carries a token: %v", c)
	}
}

func TestAnExpiredFlowIsRefused(t *testing.T) {
	env := newEnv(t)
	authorize, _, err := env.svc.Start(t.Context(), StartRequest{User: alice, SessionSHA256: aliceSess, Connector: kindCustom, Name: mockName, URL: env.vendor.MCPURL()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	code, state, _ := env.vendor.Consent(authorize)
	env.exec(t, "UPDATE connect_flows SET expires_at = now() - interval '1 second'")
	if _, err := env.svc.Complete(t.Context(), alice, aliceSess, state, code); status.Code(err) != codes.PermissionDenied {
		t.Errorf("expired flow: %v", err)
	}
}

func TestClientMetadataDocumentReplacesRegistration(t *testing.T) {
	env := newEnv(t)
	env.vendor.CIMD = true
	authorize, _, err := env.svc.Start(t.Context(), StartRequest{User: alice, SessionSHA256: aliceSess, Connector: kindCustom, Name: mockName, URL: env.vendor.MCPURL()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := mustQuery(t, authorize).Get("client_id"); got != publicURL+ClientMetadataPath {
		t.Errorf("client_id = %q", got)
	}
	code, state, _ := env.vendor.Consent(authorize)
	if _, err := env.svc.Complete(t.Context(), alice, aliceSess, state, code); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rec := httptest.NewRecorder()
	env.svc.ServeClientMetadata(rec, httptest.NewRequest(http.MethodGet, ClientMetadataPath, nil))
	var doc struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc.ClientID != publicURL+ClientMetadataPath || doc.RedirectURIs[0] != callbackURL || doc.AuthMethod != authNone {
		t.Errorf("client metadata = %s", rec.Body)
	}
}

func TestAToolsRefusalReachesTheConnector(t *testing.T) {
	env := newEnv(t)
	env.vendor.Refusal = `{"error": "the account is not enrolled in the preview"}`
	env.connect(t)
	list, err := env.svc.List(auth.WithUser(t.Context(), alice), &controlv1.ListConnectorsRequest{WithTools: true})
	if err != nil || len(list.GetConnectors()) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if got := list.GetConnectors()[0].GetToolsError(); got != `the MCP server answered HTTP 403: {"error": "the account is not enrolled in the preview"}` {
		t.Errorf("tools error = %q", got)
	}
	if g := env.grant(t); g.status != statusActive {
		t.Errorf("status = %s", g.status)
	}
}

func TestModeBServesAskToolsAsDestructiveWithoutAGate(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	tools, err := env.svc.doorTools(t.Context(), modeB)
	if err != nil || len(tools) != 2 {
		t.Fatalf("Tools = %v, %v", tools, err)
	}
	hints := map[string]string{}
	for _, tl := range tools {
		var name string
		_ = json.Unmarshal(tl["name"], &name)
		hints[name] = string(tl["annotations"])
	}
	if hints[listItems] != `{"readOnlyHint":true}` || !strings.Contains(hints[createItem], `"destructiveHint":true`) {
		t.Errorf("hints = %v", hints)
	}
	for _, name := range []string{listItems, createItem} {
		if out, err := call(t.Context(), env.svc, modeB, name, jsontext.Value(`{"title":"x"}`), ""); err != nil || out.IsError {
			t.Errorf("Call %s = %+v, %v", name, out, err)
		}
	}
	_, _, calls := env.vendor.Snapshot()
	if len(calls) != 2 || !strings.HasPrefix(calls[0].Auth, "Bearer at-") {
		t.Errorf("vendor calls = %+v", calls)
	}
}

func TestBoxDoorAsksAndAnApprovalCoversOneCallWithTheSameArguments(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	ctx := auth.WithUser(t.Context(), alice)
	if out, err := call(t.Context(), env.svc, modeA, listItems, nil, ""); err != nil || out.IsError {
		t.Fatalf("read from a box = %+v, %v", out, err)
	}
	args := jsontext.Value(`{"title":"Fix it","body":"now"}`)
	out, err := call(t.Context(), env.svc, modeA, createItem, args, "")
	if err != nil || !out.IsError || !strings.Contains(out.Content[0].Text, `mock: create_item (body="now", title="Fix it")`) {
		t.Fatalf("write from a box = %+v, %v", out, err)
	}
	approvalID := out.Meta[approvalMeta].(map[string]any)["approval_id"].(string)
	if out, _ := call(t.Context(), env.svc, modeA, createItem, args, approvalID); !strings.Contains(out.Content[0].Text, "not answered") {
		t.Errorf("retry before the answer = %+v", out)
	}
	if _, err := env.svc.Approve(auth.WithUser(t.Context(), bob), &controlv1.ApproveRequest{ApprovalId: approvalID, Decision: controlv1.ApprovalDecision_APPROVAL_DECISION_ONCE}); status.Code(err) != codes.NotFound {
		t.Errorf("bob approving alice's call: %v", err)
	}
	if _, err := env.svc.Approve(ctx, &controlv1.ApproveRequest{ApprovalId: approvalID, Decision: controlv1.ApprovalDecision_APPROVAL_DECISION_ONCE}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if out, _ := call(t.Context(), env.svc, modeA, createItem, jsontext.Value(`{"title":"Fix all","body":"now"}`), approvalID); !strings.Contains(out.Content[0].Text, "other arguments") {
		t.Errorf("changed arguments = %+v", out)
	}
	if out, err := call(t.Context(), env.svc, modeA, createItem, jsontext.Value(`{"body":"now", "title":"Fix it"}`), approvalID); err != nil || out.IsError {
		t.Errorf("approved retry = %+v, %v", out, err)
	}
	if out, _ := call(t.Context(), env.svc, modeA, createItem, args, approvalID); !strings.Contains(out.Content[0].Text, "used already") {
		t.Errorf("second use = %+v", out)
	}
	_, _, calls := env.vendor.Snapshot()
	if len(calls) != 2 {
		t.Errorf("vendor ran %d calls, want 2", len(calls))
	}
	out, _ = call(t.Context(), env.svc, modeA, createItem, args, "")
	approvalID = out.Meta[approvalMeta].(map[string]any)["approval_id"].(string)
	if _, err := env.svc.Approve(ctx, &controlv1.ApproveRequest{ApprovalId: approvalID, Decision: controlv1.ApprovalDecision_APPROVAL_DECISION_ALWAYS}); err != nil {
		t.Fatalf("Approve always: %v", err)
	}
	if out, err := call(t.Context(), env.svc, modeA, createItem, jsontext.Value(`{"title":"other"}`), ""); err != nil || out.IsError {
		t.Errorf("call after always = %+v, %v", out, err)
	}
	activity, err := env.svc.Activity(ctx, &controlv1.ActivityRequest{ConnectorId: mockName})
	if err != nil || len(activity.GetEntries()) != 8 {
		t.Fatalf("Activity = %d entries, %v", len(activity.GetEntries()), err)
	}
	for _, e := range activity.GetEntries() {
		if len(e.GetArgsHash()) != 64 || strings.Contains(e.String(), "Fix it") {
			t.Errorf("audit entry = %v", e)
		}
	}
}

func TestADeniedOrExpiredApprovalRefuses(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	ctx := auth.WithUser(t.Context(), alice)
	out, _ := call(t.Context(), env.svc, modeA, createItem, nil, "")
	denied := out.Meta[approvalMeta].(map[string]any)["approval_id"].(string)
	if _, err := env.svc.Approve(ctx, &controlv1.ApproveRequest{ApprovalId: denied, Decision: controlv1.ApprovalDecision_APPROVAL_DECISION_DENY}); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if out, _ := call(t.Context(), env.svc, modeA, createItem, nil, denied); !strings.Contains(out.Content[0].Text, "denied") {
		t.Errorf("denied call = %+v", out)
	}
	out, _ = call(t.Context(), env.svc, modeA, createItem, nil, "")
	expired := out.Meta[approvalMeta].(map[string]any)["approval_id"].(string)
	if _, err := env.svc.Approve(ctx, &controlv1.ApproveRequest{ApprovalId: expired, Decision: controlv1.ApprovalDecision_APPROVAL_DECISION_ONCE}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	env.exec(t, "UPDATE connect_approvals SET expires_at = now() - interval '1 second' WHERE id = $1", expired)
	if out, _ := call(t.Context(), env.svc, modeA, createItem, nil, expired); !strings.Contains(out.Content[0].Text, "expired") {
		t.Errorf("expired approval = %+v", out)
	}
	if _, _, calls := env.vendor.Snapshot(); len(calls) != 0 {
		t.Errorf("vendor ran %d calls, want 0", len(calls))
	}
}

func TestConcurrentCallsShareOneRefreshThatStoresTheRotatedToken(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	before := env.secrets(t)
	env.vendor.Delay = 200 * time.Millisecond
	env.exec(t, "UPDATE connect_grants SET token_expires_at = now() - interval '1 minute'")
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if out, err := call(context.Background(), env.svc, modeB, listItems, nil, ""); err != nil || out.IsError {
				t.Errorf("Call = %+v, %v", out, err)
			}
		})
	}
	wg.Wait()
	refreshes, _, _ := env.vendor.Snapshot()
	after := env.secrets(t)
	if refreshes != 1 || after.RefreshToken == before.RefreshToken || after.AccessToken == before.AccessToken {
		t.Errorf("refreshes = %d, refresh token rotated %t", refreshes, after.RefreshToken != before.RefreshToken)
	}
	other := newService(t, env.dsn)
	env.exec(t, "UPDATE connect_grants SET token_expires_at = now() - interval '1 minute'")
	wg.Go(func() { _, _ = call(context.Background(), env.svc, modeB, listItems, nil, "") })
	wg.Go(func() { _, _ = call(context.Background(), other, modeB, listItems, nil, "") })
	wg.Wait()
	if refreshes, _, _ := env.vendor.Snapshot(); refreshes != 2 {
		t.Errorf("two replicas made %d refreshes in all, want 2", refreshes)
	}
}

func TestARefusedCredentialIsRefreshedOnceAndALostGrantNeedsAReconnect(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	env.vendor.ExpireAccess()
	if out, err := call(t.Context(), env.svc, modeB, listItems, nil, ""); err != nil || out.IsError {
		t.Fatalf("Call after the vendor expired the token = %+v, %v", out, err)
	}
	env.vendor.ExpireAccess()
	env.vendor.RejectRefresh()
	out, err := call(t.Context(), env.svc, modeB, listItems, nil, "")
	if err != nil || !strings.Contains(out.Content[0].Text, "Reconnect mock") {
		t.Fatalf("Call after invalid_grant = %+v, %v", out, err)
	}
	if g := env.grant(t); g.status != statusReconnect {
		t.Errorf("status = %s", g.status)
	}
	if tools, _ := env.svc.doorTools(t.Context(), modeB); len(tools) != 0 {
		t.Errorf("a lost grant serves %d tools", len(tools))
	}
}

func TestAnUncertainWriteIsNotRetried(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	out, err := call(t.Context(), env.svc, modeB, createItem, jsontext.Value(`{"fail":"500"}`), "")
	if err != nil || !out.IsError || !strings.HasPrefix(out.Content[0].Text, outcomeUnknown) {
		t.Fatalf("Call = %+v, %v", out, err)
	}
	if _, _, calls := env.vendor.Snapshot(); len(calls) != 1 {
		t.Errorf("vendor ran %d calls, want 1", len(calls))
	}
}

func TestEventStreamAnswersAreRead(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	env.vendor.SSE = true
	if out, err := call(t.Context(), env.svc, modeB, listItems, nil, ""); err != nil || out.Content[0].Text != "list_items done" {
		t.Errorf("Call = %+v, %v", out, err)
	}
}

func TestPolicyOffHidesAndRefusesTheTool(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	ctx := auth.WithUser(t.Context(), alice)
	if _, err := env.svc.SetPolicy(ctx, &controlv1.SetPolicyRequest{ConnectorId: mockName, Tool: "list_items", Policy: controlv1.Policy_POLICY_OFF}); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if tools, _ := env.svc.doorTools(t.Context(), modeB); len(tools) != 1 {
		t.Errorf("Tools = %d, want 1", len(tools))
	}
	if out, _ := call(t.Context(), env.svc, modeB, listItems, nil, ""); !out.IsError || !strings.Contains(out.Content[0].Text, "off") {
		t.Errorf("Call = %+v", out)
	}
	if _, err := env.svc.SetPolicy(ctx, &controlv1.SetPolicyRequest{ConnectorId: mockName, Tool: "list_items"}); err != nil {
		t.Fatalf("SetPolicy default: %v", err)
	}
	if g := env.grant(t); len(g.policy) != 0 {
		t.Errorf("policy = %v", g.policy)
	}
	if _, err := env.svc.SetPolicy(auth.WithUser(t.Context(), bob), &controlv1.SetPolicyRequest{ConnectorId: mockName, Tool: "list_items", Policy: controlv1.Policy_POLICY_ALLOW}); status.Code(err) != codes.NotFound {
		t.Errorf("bob's SetPolicy: %v", err)
	}
}

func TestDisconnectRevokesDeletesAndAnnouncesTheChange(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	sec := env.secrets(t)
	changes, stop := env.svc.watchers.add(alice)
	defer stop()
	resp, err := env.svc.Disconnect(auth.WithUser(t.Context(), alice), &controlv1.ConnectorRef{ConnectorId: mockName})
	if err != nil || !resp.GetRevoked() {
		t.Fatalf("Disconnect = %v, %v", resp, err)
	}
	if _, revoked, _ := env.vendor.Snapshot(); len(revoked) != 1 || revoked[0] != sec.RefreshToken {
		t.Errorf("revoked %v", revoked)
	}
	select {
	case <-changes:
	default:
		t.Error("no list_changed")
	}
	if tools, _ := env.svc.doorTools(t.Context(), modeB); len(tools) != 0 {
		t.Errorf("Tools after disconnect = %d", len(tools))
	}
	if _, err := env.svc.Disconnect(auth.WithUser(t.Context(), alice), &controlv1.ConnectorRef{ConnectorId: mockName}); status.Code(err) != codes.NotFound {
		t.Errorf("second Disconnect: %v", err)
	}
}

func TestAKeyConnectorSendsItsHeader(t *testing.T) {
	env := newEnv(t)
	req := StartRequest{User: alice, SessionSHA256: aliceSess, Connector: kindCustom, Name: "keyed", URL: env.vendor.KeyedURL(), Header: "X-Api-Key", Key: "wrong"}
	if _, _, err := env.svc.Start(t.Context(), req); status.Code(err) != codes.InvalidArgument {
		t.Errorf("wrong key: %v", err)
	}
	req.Key = connecttest.Key
	if target, id, err := env.svc.Start(t.Context(), req); err != nil || target != "" || id != "keyed" {
		t.Fatalf("Start = %q, %q, %v", target, id, err)
	}
	if out, err := call(t.Context(), env.svc, modeB, "keyed_list_items", nil, ""); err != nil || out.IsError {
		t.Errorf("Call = %+v, %v", out, err)
	}
	if revoked, err := env.svc.Remove(t.Context(), alice, "keyed", DoorConsole); err != nil || revoked {
		t.Errorf("Remove = %t, %v", revoked, err)
	}
}

func TestStartRefusesBadCustomConnectors(t *testing.T) {
	env := newEnv(t)
	for name, req := range map[string]StartRequest{
		"catalog name": {Name: "github", URL: env.vendor.MCPURL()},
		"bad name":     {Name: "My_Tool", URL: env.vendor.MCPURL()},
		"no url":       {Name: "x"},
		"credentials":  {Name: "x", URL: "https://user:pw@example.com/mcp"},
		"header":       {Name: "x", URL: env.vendor.KeyedURL(), Header: "Bad Header", Key: "k"},
	} {
		req.User, req.SessionSHA256, req.Connector = alice, aliceSess, kindCustom
		if _, _, err := env.svc.Start(t.Context(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCatalogClientsBuildTheirAuthorizeURLs(t *testing.T) {
	env := newEnv(t)
	env.svc.catalog = newCatalog(Credentials{ClientID: "Iv1.gh", ClientSecret: "s", Slug: "cocoonbox"}, Credentials{ClientID: "g.apps", ClientSecret: "s"})
	gh, _, err := env.svc.Start(t.Context(), StartRequest{User: alice, SessionSHA256: aliceSess, Connector: "github"})
	if err != nil {
		t.Fatalf("github: %v", err)
	}
	q := mustQuery(t, gh)
	if !strings.HasPrefix(gh, "https://github.com/login/oauth/authorize?") || q.Get("client_id") != "Iv1.gh" || q.Get("redirect_uri") != callbackURL ||
		q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Has("scope") {
		t.Errorf("github = %s", gh)
	}
	google, _, err := env.svc.Start(t.Context(), StartRequest{User: alice, SessionSHA256: aliceSess, Connector: "google-calendar"})
	if err != nil {
		t.Fatalf("google: %v", err)
	}
	q = mustQuery(t, google)
	if !strings.HasPrefix(google, "https://accounts.google.com/o/oauth2/v2/auth?") || q.Get("client_id") != "g.apps" || q.Get("access_type") != "offline" ||
		q.Get("scope") != "openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/calendar.events" || q.Get("code_challenge") == "" {
		t.Errorf("google = %s", google)
	}
	catalog, _ := env.svc.Catalog(t.Context(), &controlv1.CatalogRequest{})
	if catalog.GetEntries()[0].GetInstallUrl() != "https://github.com/apps/cocoonbox/installations/new" {
		t.Errorf("install url = %q", catalog.GetEntries()[0].GetInstallUrl())
	}
	env.svc.catalog = newCatalog(Credentials{}, Credentials{})
	if _, _, err := env.svc.Start(t.Context(), StartRequest{User: alice, SessionSHA256: aliceSess, Connector: "github"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("unconfigured github: %v", err)
	}
}

func TestSealedGrantsHoldNoTokenAndOpenOnlyOnTheirRow(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	sec := env.secrets(t)
	var sealed []byte
	if err := env.row(t, "SELECT sealed FROM connect_grants").Scan(&sealed); err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(sealed), sec.AccessToken) || strings.Contains(string(sealed), sec.RefreshToken) {
		t.Error("the sealed column holds a token")
	}
	g := env.grant(t)
	var out secrets
	if err := env.svc.seal.open(t.Context(), alice, g.sealed, grantData(g.id, alice, "other"), &out); err == nil {
		t.Error("a grant opened under another connector")
	}
	if err := env.svc.seal.open(t.Context(), bob, g.sealed, grantData(g.id, bob, g.connector), &out); err == nil {
		t.Error("a grant opened under another user's key")
	}
}

func TestRefusePrivateAddresses(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:443": false, "10.203.0.1:18090": false, "169.254.169.254:80": false, "[::1]:443": false,
		"100.64.1.1:443": false, "[::ffff:192.168.1.1]:443": false, "8.8.8.8:443": true, "[2606:4700::1111]:443": true,
	} {
		if err := refusePrivate("tcp", addr, nil); (err == nil) != ok {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

func TestDoorServesMCPAndStreamsListChanged(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	door := env.svc.NewDoor(fakeAuthn{}, DoorMCP)
	srv := httptest.NewServer(door)
	t.Cleanup(srv.Close)
	if resp := post(t, srv.URL, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d", resp.StatusCode)
	}
	resp := post(t, srv.URL, "Bearer alice", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude-code"}}}`)
	session := resp.Header.Get(headerSession)
	var init struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools struct {
					ListChanged bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.UnmarshalRead(resp.Body, &init); err != nil || session == "" || init.Result.ProtocolVersion != "2025-06-18" || !init.Result.Capabilities.Tools.ListChanged {
		t.Fatalf("initialize = %+v, %v", init, err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"mock_list_items","arguments":{}}}`))
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set(headerSession, session)
	callResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var call struct {
		Result callResult `json:"result"`
	}
	if err := json.UnmarshalRead(callResp.Body, &call); err != nil || call.Result.Content[0].Text != "list_items done" {
		t.Errorf("tools/call = %+v, %v", call, err)
	}
	activity, _ := env.svc.Activity(auth.WithUser(t.Context(), alice), &controlv1.ActivityRequest{})
	if got := activity.GetEntries()[0].GetCaller(); got != "mcp:claude-code" {
		t.Errorf("caller = %q", got)
	}
	streamReq, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	streamReq.Header.Set("Authorization", "Bearer alice")
	streamReq.Header.Set("Accept", "text/event-stream")
	stream, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()
	if _, err := env.svc.Remove(t.Context(), alice, mockName, DoorConsole); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	buf := make([]byte, 256)
	n, _ := stream.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "notifications/tools/list_changed") {
		t.Errorf("stream = %q", buf[:n])
	}
}

func TestBoxDoorTakesTheApprovalFromMeta(t *testing.T) {
	env := newEnv(t)
	env.connect(t)
	srv := httptest.NewServer(env.svc.NewDoor(fakeAuthn{}, DoorBox))
	t.Cleanup(srv.Close)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mock_create_item","arguments":{"title":"x"}%s}}`
	var first struct {
		Result callResult `json:"result"`
	}
	if err := json.UnmarshalRead(post(t, srv.URL, "Bearer alice", fmt.Sprintf(body, "")).Body, &first); err != nil || !first.Result.IsError {
		t.Fatalf("first call = %+v, %v", first, err)
	}
	approvalID := first.Result.Meta[approvalMeta].(map[string]any)["approval_id"].(string)
	if _, err := env.svc.Approve(auth.WithUser(t.Context(), alice), &controlv1.ApproveRequest{ApprovalId: approvalID, Decision: controlv1.ApprovalDecision_APPROVAL_DECISION_ONCE}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	var retry struct {
		Result callResult `json:"result"`
	}
	meta := `,"_meta":{"` + approvalIDMeta + `":"` + approvalID + `"}`
	if err := json.UnmarshalRead(post(t, srv.URL, "Bearer alice", fmt.Sprintf(body, meta)).Body, &retry); err != nil || retry.Result.IsError || retry.Result.Content[0].Text != "create_item done" {
		t.Errorf("approved retry = %+v, %v", retry, err)
	}
}

type env struct {
	svc    *Service
	vendor *connecttest.Vendor
	dsn    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := freshDatabase(t)
	v := connecttest.New()
	t.Cleanup(v.Close)
	return &env{svc: newService(t, dsn), vendor: v, dsn: dsn}
}

// connect connects the vendor's OAuth MCP server as alice's custom connector "mock".
func (e *env) connect(t *testing.T) {
	t.Helper()
	authorize, _, err := e.svc.Start(t.Context(), StartRequest{User: alice, SessionSHA256: aliceSess, Connector: kindCustom, Name: mockName, URL: e.vendor.MCPURL()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	code, state, err := e.vendor.Consent(authorize)
	if err != nil {
		t.Fatalf("consent: %v", err)
	}
	if _, err := e.svc.Complete(t.Context(), alice, aliceSess, state, code); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func (e *env) grant(t *testing.T) *grant {
	t.Helper()
	g, err := e.svc.store.grant(t.Context(), alice, mockName)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return g
}

func (e *env) secrets(t *testing.T) *secrets {
	t.Helper()
	sec, err := e.svc.secrets(t.Context(), e.grant(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return sec
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.svc.store.pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) row(t *testing.T, sql string) pgx.Row {
	t.Helper()
	return e.svc.store.pool.QueryRow(t.Context(), sql)
}

func newService(t *testing.T, dsn string) *Service {
	t.Helper()
	svc, err := Open(t.Context(), Options{DSN: dsn, Keys: testKeys, PublicURL: publicURL, AllowPrivate: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

type keyFunc func(ctx context.Context, user string) (keyring.Key, error)

func (f keyFunc) StateKey(ctx context.Context, user string) (keyring.Key, error) { return f(ctx, user) }

type fakeAuthn struct{}

func (fakeAuthn) Authenticate(header string) (string, error) {
	if header == "Bearer alice" {
		return alice, nil
	}
	return "", status.Error(codes.Unauthenticated, "invalid bearer token")
}

func freshDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("COCOONBOX_TEST_DSN")
	if base == "" {
		t.Skip("COCOONBOX_TEST_DSN names no PostgreSQL server")
	}
	conn, err := pgx.Connect(t.Context(), base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	name := "t_" + randomHex(6)
	if _, err := conn.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), base); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse COCOONBOX_TEST_DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// call runs doorCall and decodes its result the way a client does.
func call(ctx context.Context, s *Service, c caller, name string, args jsontext.Value, approvalID string) (*callResult, error) {
	v, err := s.doorCall(ctx, c, name, args, approvalID)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := &callResult{}
	return out, json.Unmarshal(raw, out)
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}

func post(t *testing.T, target, authorization, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
