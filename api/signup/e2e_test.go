package signup_test

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/console"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/gwkeys"
	"github.com/loomhand/cocoonbox/api/oauth"
	"github.com/loomhand/cocoonbox/api/signup"
	"github.com/loomhand/cocoonbox/api/users"
)

const (
	alicePassword = "alice-password"
	adminPassword = "admin-password"
)

var people = map[string]person{
	"alice":   {id: 1001, login: "alice-gh", email: "alice@example.com", verified: true},
	"mallory": {id: 1002, login: "mallory", email: "mallory@example.com", verified: true},
	"bob":     {id: 2001, login: "Bob", email: "bob@example.com", verified: true},
	"carol":   {id: 2002, login: "carol", email: "carol@example.com", verified: true},
	"dave":    {id: 2003, login: "dave", email: "dave@example.com", verified: true},
	"eve":     {id: 2004, login: "eve", email: "eve@example.com", verified: false},
	"frank":   {id: 2005, login: "frank", email: "frank@other.org", verified: true},
	"bob2":    {id: 2006, login: "BOB", email: "bob2@example.com", verified: true},
	"gina":    {id: 2007, login: "gina", email: "gina@example.com", verified: true},
}

func TestALinkedGitHubAccountSignsInAndNoEmailEverLinksOne(t *testing.T) {
	e := newEnv(t)
	alice := e.browser(t)
	alice.password(t, "alice", alicePassword)
	if loc := alice.githubAs(t, "alice", "/cb/api/me/identities/github", nil); loc != "/cb/?linked=github" {
		t.Fatalf("link ends at %q", loc)
	}
	var ids []signup.Identity
	alice.json(t, http.MethodGet, "me/identities", nil, &ids)
	if len(ids) != 1 || ids[0].IDP != "github" || ids[0].Email != "alice@example.com" {
		t.Fatalf("identities = %+v", ids)
	}
	other := e.browser(t)
	other.password(t, "admin", adminPassword)
	if loc := other.githubAs(t, "alice", "/cb/api/me/identities/github", nil); !strings.Contains(loc, "login_error=github_taken") {
		t.Errorf("a second account linking the same GitHub account ends at %q", loc)
	}
	fresh := e.browser(t)
	if loc := fresh.githubAs(t, "alice", "/cb/api/auth/github/begin", map[string]string{"return_to": "/cb/oauth/authorize?client_id=x"}); loc != "/cb/oauth/authorize?client_id=x" {
		t.Fatalf("sign-in ends at %q, want the pending authorize page", loc)
	}
	var me struct {
		User users.User `json:"user"`
	}
	fresh.json(t, http.MethodGet, "me", nil, &me)
	if me.User.Name != "alice" {
		t.Fatalf("signed in as %+v, want alice", me.User)
	}
	if loc := e.browser(t).githubAs(t, "alice", "/cb/api/auth/github/begin", map[string]string{"return_to": "//evil.example/x"}); loc != "/cb/" {
		t.Errorf("a sign-in with a return path off the console ends at %q, want /cb/", loc)
	}
	if loc := e.browser(t).githubAs(t, "mallory", "/cb/api/auth/github/begin", nil); !strings.Contains(loc, "login_error=signup_closed") {
		t.Errorf("an unknown GitHub account with signup off ends at %q", loc)
	}
	if status, _ := alice.call(t, http.MethodDelete, "me/identities/github", map[string]any{}); status != http.StatusNoContent {
		t.Fatalf("unlink: %d", status)
	}
	if loc := e.browser(t).githubAs(t, "alice", "/cb/api/auth/github/begin", nil); !strings.Contains(loc, "login_error=") {
		t.Errorf("an unlinked GitHub account still signs in: %q", loc)
	}
	stray := e.browser(t)
	if loc := stray.callback(t, "alice", "nostate"); !strings.Contains(loc, "login_error=browser") {
		t.Errorf("a callback without the login cookie ends at %q", loc)
	}
	alice.githubAs(t, "alice", "/cb/api/me/identities/github", nil)
	retry := e.browser(t)
	state := retry.begin(t, "/cb/api/auth/github/begin", nil)
	if loc := retry.callback(t, "alice", "forged"); !strings.Contains(loc, "login_error=failed") {
		t.Errorf("a callback with another state ends at %q", loc)
	}
	if loc := retry.callback(t, "alice", state); loc != "/cb/" {
		t.Errorf("after a forged state the real answer ends at %q, want signed in", loc)
	}
}

func TestLinkingAQueuedGitHubAccountDropsItsPlace(t *testing.T) {
	e := newEnv(t)
	admin := e.browser(t)
	admin.password(t, "admin", adminPassword)
	admin.setSignup(t, signup.Settings{Enabled: true, Seats: 1, DailyCapMicros: 1_000_000, MaxAgentBoxes: 1, HourlyAccounts: 50})
	if loc := e.browser(t).githubAs(t, "mallory", "/cb/api/auth/github/begin", nil); loc != "/cb/queue" {
		t.Fatalf("mallory: %q", loc)
	}
	alice := e.browser(t)
	alice.password(t, "alice", alicePassword)
	if loc := alice.githubAs(t, "mallory", "/cb/api/me/identities/github", nil); loc != "/cb/?linked=github" {
		t.Fatalf("link of a queued GitHub account ends at %q", loc)
	}
	admin.setSignup(t, signup.Settings{Enabled: true, Seats: 5, DailyCapMicros: 1_000_000, MaxAgentBoxes: 1, HourlyAccounts: 50})
	var accounts int
	if err := query(t, e.dsn, "SELECT count(*) FROM users WHERE idp = 'github'").Scan(&accounts); err != nil || accounts != 0 {
		t.Errorf("signup accounts after the link and a raised cap = %d, %v, want 0", accounts, err)
	}
}

func TestInviteOnlySignupAdmitsAtOnceThenQueuesAndAFreedSeatAdmitsTheFront(t *testing.T) {
	e := newEnv(t)
	admin := e.browser(t)
	admin.password(t, "admin", adminPassword)
	admin.setSignup(t, signup.Settings{Enabled: true, InviteOnly: true, Seats: 2, DailyCapMicros: 1_000_000, MaxAgentBoxes: 1, HourlyAccounts: 50})
	if loc := e.browser(t).githubAs(t, "bob", "/cb/api/auth/github/begin", nil); !strings.Contains(loc, "login_error=invite_required") {
		t.Fatalf("invite-only signup without an invite ends at %q", loc)
	}
	var made struct {
		Code string `json:"code"`
	}
	admin.json(t, http.MethodPost, "invites", map[string]int{"uses": 3, "days": 7}, &made)
	bob := e.browser(t)
	if loc := bob.githubAs(t, "bob", "/cb/api/auth/github/begin", map[string]string{"invite": made.Code}); loc != "/cb/" {
		t.Fatalf("an invited signup with a free seat ends at %q, want signed in", loc)
	}
	b, err := e.users.ByName("bob")
	if err != nil || b.IDP != "github" || b.MaxAgentBoxes == nil || *b.MaxAgentBoxes != 1 || e.gw.cap(b.ID) != 1_000_000 {
		t.Fatalf("bob = %+v, %v; gw cap %d", b, err, e.gw.cap(b.ID))
	}
	carol := e.browser(t)
	if loc := carol.githubAs(t, "carol", "/cb/api/auth/github/begin", map[string]string{"invite": made.Code}); loc != "/cb/queue" {
		t.Fatalf("an invited signup with every seat taken ends at %q, want the queue", loc)
	}
	admin.setSignup(t, signup.Settings{Enabled: true, InviteOnly: false, Seats: 2, AllowDomains: []string{"Example.com"}, DailyCapMicros: 1_000_000, MaxAgentBoxes: 1, HourlyAccounts: 50})
	dave := e.browser(t)
	if loc := dave.githubAs(t, "dave", "/cb/api/auth/github/begin", nil); loc != "/cb/queue" {
		t.Fatalf("an open signup with every seat taken ends at %q", loc)
	}
	gina := e.browser(t)
	if loc := gina.githubAs(t, "gina", "/cb/api/auth/github/begin", map[string]string{"invite": made.Code}); loc != "/cb/queue" {
		t.Fatalf("the invite's last use ends at %q", loc)
	}
	for name, want := range map[string]int{"carol": 2, "gina": 1, "dave": 3} {
		var place signup.Place
		map[string]*browser{"carol": carol, "gina": gina, "dave": dave}[name].json(t, http.MethodGet, "queue/me", nil, &place)
		if place.Status != "waiting" || place.Position != want || place.Waiting != 3 {
			t.Errorf("%s's place = %+v, want %d of 3 (an invite goes to the front)", name, place, want)
		}
	}
	if loc := e.browser(t).githubAs(t, "eve", "/cb/api/auth/github/begin", nil); !strings.Contains(loc, "login_error=email_unverified") {
		t.Errorf("an unverified email ends at %q", loc)
	}
	if loc := e.browser(t).githubAs(t, "frank", "/cb/api/auth/github/begin", nil); !strings.Contains(loc, "login_error=email_not_allowed") {
		t.Errorf("an email outside the allowlist ends at %q", loc)
	}
	if status, _ := bob.call(t, http.MethodPost, "me/delete", map[string]string{"name": "nobody"}); status != http.StatusBadRequest {
		t.Errorf("a self-delete with the wrong name: %d", status)
	}
	if status, _ := bob.call(t, http.MethodPost, "me/delete", map[string]string{"name": "bob"}); status != http.StatusNoContent {
		t.Fatalf("self-delete: %d", status)
	}
	if _, err := e.users.ByName("bob"); err == nil {
		t.Error("bob still exists after the self-delete")
	}
	var place signup.Place
	gina.json(t, http.MethodGet, "queue/me", nil, &place)
	if place.Status != "admitted" {
		t.Fatalf("after bob's seat freed, gina's place = %+v, want admitted", place)
	}
	carol.json(t, http.MethodGet, "queue/me", nil, &place)
	if place.Status != "waiting" || place.Position != 1 {
		t.Errorf("carol's place after one freed seat = %+v, want still waiting, first", place)
	}
	if loc := gina.githubAs(t, "gina", "/cb/api/auth/github/begin", nil); loc != "/cb/" {
		t.Errorf("an admitted person's next sign-in ends at %q", loc)
	}
	if status, _ := dave.call(t, http.MethodDelete, "queue/me", map[string]any{}); status != http.StatusNoContent {
		t.Errorf("leave the queue: %d", status)
	}
	var queue []signup.Entry
	admin.json(t, http.MethodGet, "queue", nil, &queue)
	if len(queue) != 2 || queue[0].Email != "carol@example.com" || queue[0].Position != 1 || queue[1].Status != "admitted" {
		t.Fatalf("queue = %+v, want carol waiting and gina admitted; dave's row gone", queue)
	}
	if status, _ := admin.call(t, http.MethodPost, "queue/"+queue[0].ID+"/reject", map[string]any{}); status != http.StatusNoContent {
		t.Fatalf("reject: %d", status)
	}
	if loc := e.browser(t).githubAs(t, "carol", "/cb/api/auth/github/begin", nil); !strings.Contains(loc, "login_error=declined") {
		t.Errorf("a declined person's sign-in ends at %q", loc)
	}
}

func TestAnAdminAdmitsPastTheCapAndUsernamesTakeASuffix(t *testing.T) {
	e := newEnv(t)
	admin := e.browser(t)
	admin.password(t, "admin", adminPassword)
	admin.setSignup(t, signup.Settings{Enabled: true, Seats: 2, DailyCapMicros: 1_000_000, MaxAgentBoxes: 1, HourlyAccounts: 50})
	if loc := e.browser(t).githubAs(t, "bob", "/cb/api/auth/github/begin", nil); loc != "/cb/" {
		t.Fatalf("bob: %q", loc)
	}
	if loc := e.browser(t).githubAs(t, "bob2", "/cb/api/auth/github/begin", nil); loc != "/cb/queue" {
		t.Fatalf("bob2: %q", loc)
	}
	var queue []signup.Entry
	admin.json(t, http.MethodGet, "queue", nil, &queue)
	if status, body := admin.call(t, http.MethodPost, "queue/"+queue[0].ID+"/admit", map[string]any{}); status != http.StatusNoContent {
		t.Fatalf("admit now: %d %s", status, body)
	}
	if u, err := e.users.ByName("bob-2"); err != nil || u.IDP != "github" {
		t.Errorf("the second GitHub login that cleans to bob = %+v, %v, want bob-2", u, err)
	}
	var state struct {
		Seats signup.Seats `json:"seats"`
	}
	admin.json(t, http.MethodGet, "signup", nil, &state)
	if state.Seats.Used != 3 || state.Seats.Cap != 2 || state.Seats.Waiting != 0 {
		t.Errorf("seats = %+v, want 3 used of 2 after an admin's admit", state.Seats)
	}
}

func TestTheHourlyBrakeHoldsAdmission(t *testing.T) {
	e := newEnv(t)
	admin := e.browser(t)
	admin.password(t, "admin", adminPassword)
	admin.setSignup(t, signup.Settings{Enabled: true, Seats: 10, DailyCapMicros: 1_000_000, MaxAgentBoxes: 1, HourlyAccounts: 1})
	if loc := e.browser(t).githubAs(t, "bob", "/cb/api/auth/github/begin", nil); loc != "/cb/" {
		t.Fatalf("bob: %q", loc)
	}
	if loc := e.browser(t).githubAs(t, "dave", "/cb/api/auth/github/begin", nil); loc != "/cb/queue" {
		t.Errorf("past the hourly brake with free seats: %q, want the queue", loc)
	}
}

func TestOneFreedSeatAdmitsOnePersonWhileTwoApisAdmitAtOnce(t *testing.T) {
	e := newEnv(t)
	admin := e.browser(t)
	admin.password(t, "admin", adminPassword)
	admin.setSignup(t, signup.Settings{Enabled: true, Seats: 1, DailyCapMicros: 1_000_000, MaxAgentBoxes: 1, HourlyAccounts: 50})
	for _, name := range []string{"bob", "carol", "dave", "gina"} {
		e.browser(t).githubAs(t, name, "/cb/api/auth/github/begin", nil)
	}
	other := e.openSignup(t)
	if err := exec(t, e.dsn, "UPDATE signup_settings SET seats = seats + 1"); err != nil {
		t.Fatalf("raise seats: %v", err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		svc := []*signup.Service{e.signup, other}[i%2]
		wg.Go(func() {
			if err := svc.Admit(t.Context()); err != nil {
				t.Errorf("Admit: %v", err)
			}
		})
	}
	wg.Wait()
	var admitted int
	if err := query(t, e.dsn, "SELECT count(*) FROM signup_queue WHERE status = 'admitted'").Scan(&admitted); err != nil || admitted != 1 {
		t.Errorf("admitted = %d, %v; want the one freed seat to admit one person", admitted, err)
	}
	var accounts int
	if err := query(t, e.dsn, "SELECT count(*) FROM users WHERE idp = 'github'").Scan(&accounts); err != nil || accounts != 1 {
		t.Errorf("signup accounts = %d, %v, want 1", accounts, err)
	}
}

func TestInviteCodeOnlySignupMakesAnAccountOnlyWithAValidInviteAndWritesNothingWithout(t *testing.T) {
	e := newEnv(t)
	admin := e.browser(t)
	admin.password(t, "admin", adminPassword)
	admin.setSignup(t, signup.Settings{Enabled: true, InviteOnly: true, Seats: 100, DailyCapMicros: 1_000_000, MaxAgentBoxes: 1, HourlyAccounts: 50})
	written := func() (int, int) {
		t.Helper()
		var queued, accounts int
		if err := query(t, e.dsn, "SELECT (SELECT count(*) FROM signup_queue), (SELECT count(*) FROM users WHERE idp = 'github')").Scan(&queued, &accounts); err != nil {
			t.Fatalf("count: %v", err)
		}
		return queued, accounts
	}
	if loc := e.browser(t).githubAs(t, "bob", "/cb/api/auth/github/begin", nil); loc != "/cb/?login_error=invite_required" {
		t.Fatalf("no invite ends at %q, want the invite_required code", loc)
	}
	if loc := e.browser(t).githubAs(t, "bob", "/cb/api/auth/github/begin", map[string]string{"invite": "cbi_" + strings.Repeat("0", 64)}); loc != "/cb/?login_error=invite_required" {
		t.Fatalf("an unknown invite ends at %q", loc)
	}
	if queued, accounts := written(); queued != 0 || accounts != 0 {
		t.Fatalf("after refusals: %d queue entries, %d accounts, want none", queued, accounts)
	}
	var one struct {
		Code   string        `json:"code"`
		Invite signup.Invite `json:"invite"`
	}
	admin.json(t, http.MethodPost, "invites", map[string]int{"uses": 1, "days": 1}, &one)
	if loc := e.browser(t).githubAs(t, "bob", "/cb/api/auth/github/begin", map[string]string{"invite": one.Code}); loc != "/cb/" {
		t.Fatalf("a valid invite ends at %q, want signed in", loc)
	}
	if loc := e.browser(t).githubAs(t, "carol", "/cb/api/auth/github/begin", map[string]string{"invite": one.Code}); loc != "/cb/?login_error=invite_required" {
		t.Errorf("a used-up invite ends at %q", loc)
	}
	var expired struct {
		Code   string        `json:"code"`
		Invite signup.Invite `json:"invite"`
	}
	admin.json(t, http.MethodPost, "invites", map[string]int{"uses": 5, "days": 1}, &expired)
	if err := exec(t, e.dsn, "UPDATE signup_invites SET expires_at = now() - interval '1 second' WHERE id = $1", expired.Invite.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if loc := e.browser(t).githubAs(t, "dave", "/cb/api/auth/github/begin", map[string]string{"invite": expired.Code}); loc != "/cb/?login_error=invite_required" {
		t.Errorf("an expired invite ends at %q", loc)
	}
	var usesLeft int
	if err := query(t, e.dsn, "SELECT uses_left FROM signup_invites WHERE id = $1", expired.Invite.ID).Scan(&usesLeft); err != nil || usesLeft != 5 {
		t.Errorf("the expired invite's uses = %d, %v, want 5 untouched", usesLeft, err)
	}
	if queued, accounts := written(); queued != 1 || accounts != 1 {
		t.Errorf("after one valid and two spent invites: %d queue entries, %d accounts, want only bob's", queued, accounts)
	}
	if loc := e.browser(t).githubAs(t, "bob", "/cb/api/auth/github/begin", nil); loc != "/cb/" {
		t.Errorf("the invited account's next sign-in without an invite ends at %q", loc)
	}
	alice := e.browser(t)
	alice.password(t, "alice", alicePassword)
	alice.githubAs(t, "alice", "/cb/api/me/identities/github", nil)
	if loc := e.browser(t).githubAs(t, "alice", "/cb/api/auth/github/begin", nil); loc != "/cb/" {
		t.Errorf("an existing account with GitHub linked ends at %q, want signed in", loc)
	}
}

func TestDemotingASignupAccountRestoresTheSignupCap(t *testing.T) {
	e := newEnv(t)
	admin := e.browser(t)
	admin.password(t, "admin", adminPassword)
	admin.setSignup(t, signup.Settings{Enabled: true, Seats: 2, DailyCapMicros: 1_000_000, MaxAgentBoxes: 1, HourlyAccounts: 50})
	signedIn := e.browser(t)
	if loc := signedIn.githubAs(t, "bob", "/cb/api/auth/github/begin", nil); loc != "/cb/" {
		t.Fatalf("bob's signup ends at %q", loc)
	}
	bob, _ := e.users.ByName("bob")
	if status, _ := signedIn.call(t, http.MethodPatch, "users/"+bob.ID, map[string]bool{"admin": true}); status != http.StatusForbidden {
		t.Errorf("bob promoting himself: %d, want 403", status)
	}
	var before, after struct {
		Seats struct {
			Used int `json:"used"`
		} `json:"seats"`
	}
	admin.json(t, http.MethodGet, "signup", nil, &before)
	if status, body := admin.call(t, http.MethodPatch, "users/"+bob.ID, map[string]bool{"admin": true}); status != http.StatusOK || e.gw.cap(bob.ID) != gwkeys.Unlimited {
		t.Fatalf("promote: %d %s; gw cap %d", status, body, e.gw.cap(bob.ID))
	}
	admin.json(t, http.MethodGet, "signup", nil, &after)
	if after.Seats.Used != before.Seats.Used-1 {
		t.Errorf("seats used %d with bob an admin, %d before; an admin takes no seat", after.Seats.Used, before.Seats.Used)
	}
	if status, body := admin.call(t, http.MethodPatch, "users/"+bob.ID, map[string]bool{"admin": false}); status != http.StatusOK || e.gw.cap(bob.ID) != 1_000_000 {
		t.Fatalf("demote: %d %s; gw cap %d, want the signup cap", status, body, e.gw.cap(bob.ID))
	}
	root, _ := e.users.ByName("admin")
	if status, _ := admin.call(t, http.MethodPatch, "users/"+root.ID, map[string]bool{"admin": false}); status != http.StatusBadRequest {
		t.Errorf("demoting the last admin: %d, want 400", status)
	}
}

func TestAnInviteOnTheConsentPageSignsUpAndSignsTheCLIInInOnePass(t *testing.T) {
	e := newEnv(t)
	admin := e.browser(t)
	admin.password(t, "admin", adminPassword)
	admin.setSignup(t, signup.Settings{Enabled: true, InviteOnly: true, Seats: 10, DailyCapMicros: 1_000_000, MaxAgentBoxes: 1, HourlyAccounts: 50})
	var made struct {
		Code string `json:"code"`
	}
	admin.json(t, http.MethodPost, "invites", map[string]int{"uses": 1, "days": 1}, &made)
	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	authorize := url.Values{
		"response_type": {"code"}, "client_id": {oauth.CLIClient}, "redirect_uri": {"http://127.0.0.1:50123/callback"}, "scope": {oauth.AccountScope},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "state": {"st"}, "device_name": {"bob-mac"},
	}
	if loc := e.browser(t).githubAs(t, "carol", "/cb/api/auth/github/begin", map[string]string{"return_to": "/cb/oauth/authorize?" + authorize.Encode()}); loc != "/cb/?login_error=invite_required" {
		t.Fatalf("the consent page's sign-in without an invite ends at %q", loc)
	}
	var accounts int
	if err := query(t, e.dsn, "SELECT count(*) FROM users WHERE idp = 'github'").Scan(&accounts); err != nil || accounts != 0 {
		t.Fatalf("accounts after the refusal = %d, %v", accounts, err)
	}
	withInvite := url.Values{}
	for k, v := range authorize {
		withInvite[k] = v
	}
	withInvite.Set("invite", made.Code)
	page := "/cb/oauth/authorize?" + withInvite.Encode()
	bob := e.browser(t)
	if loc := bob.githubAs(t, "bob", "/cb/api/auth/github/begin", map[string]string{"return_to": page, "invite": withInvite.Get("invite")}); loc != page {
		t.Fatalf("the consent page's sign-in with an invite ends at %q, want back on the consent page", loc)
	}
	var consent oauth.Consent
	bob.json(t, http.MethodPost, "oauth/check", map[string]string{"query": withInvite.Encode()}, &consent)
	if !consent.Account || consent.DeviceName != "bob-mac" {
		t.Fatalf("consent for the new account = %+v", consent)
	}
	var approved struct {
		Redirect string `json:"redirect"`
	}
	bob.json(t, http.MethodPost, "oauth/approve", map[string]string{"query": withInvite.Encode()}, &approved)
	back, _ := url.Parse(approved.Redirect)
	resp, err := e.srv.Client().PostForm(e.srv.URL+oauth.TokenPath, url.Values{
		"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {oauth.CLIClient},
		"redirect_uri": {authorize.Get("redirect_uri")}, "code_verifier": {verifier},
	})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode != http.StatusOK || json.UnmarshalRead(resp.Body, &tok) != nil {
		t.Fatalf("token: %d", resp.StatusCode)
	}
	device := sha256.Sum256([]byte(tok.AccessToken))
	b, err := e.users.ByName("bob")
	if user, ok := e.users.ByTokenSHA256(t.Context(), hex.EncodeToString(device[:])); err != nil || !ok || user != b.ID || b.IDP != "github" {
		t.Errorf("the device token resolves to %q, %t; bob = %+v, %v", user, ok, b, err)
	}
}

func TestAnInviteTokenMakesOnlyOneUseInvitesAndListsOnlyItsOwn(t *testing.T) {
	e := newEnv(t)
	admin := e.browser(t)
	admin.password(t, "admin", adminPassword)
	admin.setSignup(t, signup.Settings{Enabled: true, InviteOnly: true, Seats: 10, DailyCapMicros: 1_000_000, MaxAgentBoxes: 1, HourlyAccounts: 50})
	var minted struct {
		Token       string             `json:"token"`
		InviteToken signup.InviteToken `json:"invite_token"`
	}
	admin.json(t, http.MethodPost, "invite-tokens", map[string]string{"label": "dora"}, &minted)
	if !strings.HasPrefix(minted.Token, auth.InviteTokenPrefix) || minted.InviteToken.Label != "dora" {
		t.Fatalf("minted = %+v", minted)
	}
	mintedSum := sha256.Sum256([]byte(minted.Token))
	var hashed, clear int
	if err := query(t, e.dsn, "SELECT count(*) FILTER (WHERE sha256 = $1), count(*) FILTER (WHERE sha256 = $2 OR label = $2) FROM invite_tokens",
		hex.EncodeToString(mintedSum[:]), minted.Token).Scan(&hashed, &clear); err != nil || hashed != 1 || clear != 0 {
		t.Errorf("stored as its sha256: %d, in the clear: %d, %v", hashed, clear, err)
	}
	admin.json(t, http.MethodPost, "invites", map[string]int{"uses": 3, "days": 30}, &struct{}{})
	var made struct {
		Code   string        `json:"code"`
		Invite signup.Invite `json:"invite"`
	}
	status, body := e.bearer(t, minted.Token, http.MethodPost, "/cb/api/invites", map[string]any{})
	if status != http.StatusOK || json.Unmarshal([]byte(body), &made) != nil {
		t.Fatalf("a token's invite: %d %s", status, body)
	}
	if made.Invite.UsesLeft != 1 || made.Invite.TokenID != minted.InviteToken.ID || time.Until(made.Invite.ExpiresAt) < 47*time.Hour || time.Until(made.Invite.ExpiresAt) > 49*time.Hour {
		t.Errorf("a token's invite = %+v, want one use for two days", made.Invite)
	}
	for _, bad := range []map[string]int{{"days": 8}, {"uses": 2}, {"days": -1}} {
		if status, body := e.bearer(t, minted.Token, http.MethodPost, "/cb/api/invites", bad); status != http.StatusBadRequest {
			t.Errorf("a token's invite with %v: %d %s, want 400", bad, status, body)
		}
	}
	if status, _ := e.bearer(t, minted.Token, http.MethodPost, "/cb/api/invites", map[string]int{"days": 7}); status != http.StatusOK {
		t.Errorf("a token's invite of 7 days: %d", status)
	}
	var own []signup.Invite
	status, body = e.bearer(t, minted.Token, http.MethodGet, "/cb/api/invites", nil)
	if status != http.StatusOK || json.Unmarshal([]byte(body), &own) != nil || len(own) != 2 || own[0].TokenID != minted.InviteToken.ID || own[1].TokenID != minted.InviteToken.ID {
		t.Fatalf("the token's list = %d %s, want its two invites and not the admin's", status, body)
	}
	if loc := e.browser(t).githubAs(t, "bob", "/cb/api/auth/github/begin", map[string]string{"invite": made.Code}); loc != "/cb/" {
		t.Errorf("a token's invite signs up to %q", loc)
	}
	if loc := e.browser(t).githubAs(t, "carol", "/cb/api/auth/github/begin", map[string]string{"invite": made.Code}); loc != "/cb/?login_error=invite_required" {
		t.Errorf("a token's invite used twice ends at %q", loc)
	}
	var tokens []signup.InviteToken
	admin.json(t, http.MethodGet, "invite-tokens", nil, &tokens)
	if len(tokens) != 1 || tokens[0].Invites != 2 || tokens[0].UsedAt == nil {
		t.Errorf("tokens = %+v", tokens)
	}
}

func TestAnInviteTokenIsRefusedEverywhereElseAndStopsWithItsAdmin(t *testing.T) {
	e := newEnv(t)
	admin := e.browser(t)
	admin.password(t, "admin", adminPassword)
	var minted struct {
		Token       string             `json:"token"`
		InviteToken signup.InviteToken `json:"invite_token"`
	}
	admin.json(t, http.MethodPost, "invite-tokens", map[string]string{"label": "dora"}, &minted)
	minted0 := func() string {
		t.Helper()
		var users, tokens, invites, sessions, devices int
		if err := query(t, e.dsn, `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM invite_tokens), (SELECT count(*) FROM signup_invites),
			(SELECT count(*) FROM sessions), (SELECT count(*) FROM user_tokens)`).Scan(&users, &tokens, &invites, &sessions, &devices); err != nil {
			t.Fatalf("count: %v", err)
		}
		return fmt.Sprint(users, tokens, invites, sessions, devices)
	}
	before := minted0()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/cb/"},
		{http.MethodGet, "/cb/oauth/authorize"},
		{http.MethodGet, "/cb/queue"},
		{http.MethodPost, "/cb/api/login"},
		{http.MethodPost, "/cb/api/logout"},
		{http.MethodGet, "/cb/api/me"},
		{http.MethodPatch, "/cb/api/me"},
		{http.MethodPost, "/cb/api/me/token"},
		{http.MethodPost, "/cb/api/me/password"},
		{http.MethodPost, "/cb/api/me/delete"},
		{http.MethodGet, "/cb/api/me/identities"},
		{http.MethodPost, "/cb/api/me/identities/github"},
		{http.MethodDelete, "/cb/api/me/identities/github"},
		{http.MethodGet, "/cb/api/devices"},
		{http.MethodDelete, "/cb/api/devices/dev_x"},
		{http.MethodPost, "/cb/api/boxes"},
		{http.MethodPatch, "/cb/api/boxes/cb-x"},
		{http.MethodDelete, "/cb/api/boxes/cb-x"},
		{http.MethodGet, "/cb/api/users"},
		{http.MethodPost, "/cb/api/users"},
		{http.MethodPatch, "/cb/api/users/u-x"},
		{http.MethodPost, "/cb/api/users/u-x/password"},
		{http.MethodPut, "/cb/api/users/u-x/daily-cap"},
		{http.MethodDelete, "/cb/api/users/u-x"},
		{http.MethodGet, "/cb/api/signup"},
		{http.MethodPut, "/cb/api/signup"},
		{http.MethodGet, "/cb/api/queue"},
		{http.MethodPost, "/cb/api/queue/sq_x/admit"},
		{http.MethodGet, "/cb/api/queue/me"},
		{http.MethodDelete, "/cb/api/queue/me"},
		{http.MethodDelete, "/cb/api/invites/inv_x"},
		{http.MethodPut, "/cb/api/invites"},
		{http.MethodGet, "/cb/api/invite-tokens"},
		{http.MethodPost, "/cb/api/invite-tokens"},
		{http.MethodDelete, "/cb/api/invite-tokens/" + minted.InviteToken.ID},
		{http.MethodGet, "/cb/api/auth/providers"},
		{http.MethodPost, "/cb/api/auth/github/begin"},
		{http.MethodPost, "/cb/api/oauth/check"},
		{http.MethodPost, "/cb/api/oauth/approve"},
		{http.MethodGet, "/cb/api/agents"},
		{http.MethodGet, "/cb/api/keys"},
		{http.MethodPost, "/cb/api/keys"},
		{http.MethodPost, "/cocoonbox.mcp/control"},
		{http.MethodPost, "/cocoonbox.mcp/cb-x"},
		{http.MethodPost, "/cocoonbox.mcp/connect"},
		{http.MethodGet, "/cocoonbox.connect/callback"},
		{http.MethodGet, signup.CallbackPath},
		{http.MethodPost, oauth.TokenPath},
		{http.MethodPost, "/cocoonbox.oauth/register"},
		{http.MethodGet, "/.well-known/oauth-authorization-server"},
		{http.MethodGet, "/.well-known/oauth-protected-resource/cocoonbox.mcp"},
		{http.MethodPost, "/cocoonbox.oauth/revoke"},
		{http.MethodGet, "/cocoonbox.connect/client-metadata.json"},
		{http.MethodGet, "/cb/connect"},
		{http.MethodGet, "/cb/api/connect"},
		{http.MethodPost, "/cb/api/connect/github/begin"},
		{http.MethodGet, "/cb/s/ws"},
		{http.MethodGet, "/cb/agents"},
		{http.MethodGet, "/cb/keys"},
	} {
		var body any
		if route.method != http.MethodGet {
			body = map[string]any{}
		}
		if status, _ := e.bearer(t, minted.Token, route.method, route.path, body); status != http.StatusForbidden {
			t.Errorf("%s %s with an invite token: %d, want 403", route.method, route.path, status)
		}
	}
	if after := minted0(); after != before {
		t.Errorf("users, invite tokens, invites, sessions, device tokens = %s after the refused calls, %s before; an invite token minted something", after, before)
	}
	if status, body := e.bearer(t, "cb_"+strings.Repeat("0", 64), http.MethodPost, "/cocoonbox.mcp/control", map[string]any{}); status != http.StatusOK || body != "reached" {
		t.Errorf("a request without an invite token passes the gate: %d %q", status, body)
	}
	tokenSum := sha256.Sum256([]byte(minted.Token))
	if _, ok := e.users.ByTokenSHA256(t.Context(), hex.EncodeToString(tokenSum[:])); ok {
		t.Error("an invite token passes the user token check")
	}
	if status, _ := e.bearer(t, auth.InviteTokenPrefix+strings.Repeat("0", 64), http.MethodPost, "/cb/api/invites", map[string]any{}); status != http.StatusUnauthorized {
		t.Errorf("an unknown invite token: %d, want 401", status)
	}
	other := e.browser(t)
	other.password(t, "alice", alicePassword)
	alice, _ := e.users.ByName("alice")
	if status, _ := admin.call(t, http.MethodPatch, "users/"+alice.ID, map[string]bool{"admin": true}); status != http.StatusOK {
		t.Fatalf("promote alice: %d", status)
	}
	var second struct {
		Token string `json:"token"`
	}
	other.json(t, http.MethodPost, "invite-tokens", map[string]string{"label": "alice's"}, &second)
	if status, _ := admin.call(t, http.MethodPatch, "users/"+alice.ID, map[string]bool{"admin": false}); status != http.StatusOK {
		t.Fatalf("demote alice: %d", status)
	}
	if status, _ := e.bearer(t, second.Token, http.MethodPost, "/cb/api/invites", map[string]any{}); status != http.StatusUnauthorized {
		t.Errorf("the token of a demoted admin: %d, want 401", status)
	}
	if status, _ := admin.call(t, http.MethodDelete, "invite-tokens/"+minted.InviteToken.ID, map[string]any{}); status != http.StatusNoContent {
		t.Fatalf("revoke: %d", status)
	}
	if status, _ := e.bearer(t, minted.Token, http.MethodPost, "/cb/api/invites", map[string]any{}); status != http.StatusUnauthorized {
		t.Errorf("a revoked invite token: %d, want 401", status)
	}
}

type person struct {
	id       int64
	login    string
	email    string
	verified bool
}

type env struct {
	srv    *httptest.Server
	github *httptest.Server
	users  *users.Service
	signup *signup.Service
	gw     *fakeGW
	dsn    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{dsn: freshDatabase(t), gw: &fakeGW{caps: map[string]int64{}}}
	us, err := users.Open(t.Context(), users.Options{
		DSN: e.dsn, MasterKeyFile: masterKeyFile(t), GW: e.gw, Tenants: fakeTenants{},
		FirstBoxID: func(user string) string { return "cb-" + hex.EncodeToString([]byte(user))[:12] },
	})
	if err != nil {
		t.Fatalf("open users: %v", err)
	}
	t.Cleanup(us.Close)
	e.users = us
	if _, err := us.Create(t.Context(), users.NewUser{Name: "alice", Password: alicePassword}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, err := us.Create(t.Context(), users.NewUser{Name: "admin", Password: adminPassword, Admin: true}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	e.github = newFakeGitHub(t)
	mux := http.NewServeMux()
	for _, path := range []string{"/cocoonbox.mcp/", "/cocoonbox.connect/", "/cocoonbox.oauth/register", "/cocoonbox.oauth/revoke", "/.well-known/"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("reached")) })
	}
	e.srv = httptest.NewTLSServer(console.InviteTokenGate(mux))
	t.Cleanup(e.srv.Close)
	e.signup = e.openSignup(t)
	grants, err := oauth.Open(t.Context(), oauth.Options{DSN: e.dsn, Issuer: e.srv.URL, Resource: e.srv.URL + "/cocoonbox.mcp", Accounts: us})
	if err != nil {
		t.Fatalf("open oauth: %v", err)
	}
	t.Cleanup(grants.Close)
	web := console.New(us, fakeBoxes{}, 0, 1)
	web.EnableSignup(e.signup)
	web.EnableAgents(grants)
	mux.Handle(console.Prefix, web)
	mux.HandleFunc("GET "+signup.CallbackPath, web.LoginCallback)
	mux.HandleFunc("POST "+oauth.TokenPath, grants.ServeToken)
	return e
}

// bearer sends a call under /cb/ with token as its bearer and no cookies.
func (e *env) bearer(t *testing.T, token, method, path string, body any) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = strings.NewReader(string(data))
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, e.srv.URL+path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func (e *env) openSignup(t *testing.T) *signup.Service {
	t.Helper()
	svc, err := signup.Open(t.Context(), signup.Options{
		DSN: e.dsn, PublicURL: e.srv.URL, Accounts: e.users,
		GitHub: signup.GitHubClient{
			ClientID: "gh-login", ClientSecret: "gh-secret", AuthorizeURL: e.github.URL + "/login/oauth/authorize",
			TokenURL: e.github.URL + "/login/oauth/access_token", APIURL: e.github.URL,
		},
	})
	if err != nil {
		t.Fatalf("open signup: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

// browser is one person's browser: its own cookies, and redirects left to the test.
type browser struct {
	e      *env
	client *http.Client
}

func (b *browser) password(t *testing.T, name, password string) {
	t.Helper()
	if status, body := b.call(t, http.MethodPost, "login", map[string]string{"name": name, "password": password}); status != http.StatusOK {
		t.Fatalf("sign in as %s: %d %s", name, status, body)
	}
}

// githubAs begins a sign-in at path, plays GitHub's consent for who, and returns where the callback sends the browser.
func (b *browser) githubAs(t *testing.T, who, path string, body any) string {
	t.Helper()
	return b.callback(t, who, b.begin(t, path, body))
}

// begin starts a sign-in at path and returns its state.
func (b *browser) begin(t *testing.T, path string, body any) string {
	t.Helper()
	var started struct {
		URL string `json:"url"`
	}
	status, raw := b.call(t, http.MethodPost, strings.TrimPrefix(path, "/cb/api/"), cmp.Or(body, any(map[string]any{})))
	if status != http.StatusOK || json.Unmarshal([]byte(raw), &started) != nil {
		t.Fatalf("begin at %s: %d %s", path, status, raw)
	}
	u, err := url.Parse(started.URL)
	q := u.Query()
	if err != nil || !strings.HasPrefix(started.URL, b.e.github.URL+"/login/oauth/authorize?") || q.Get("client_id") != "gh-login" || q.Get("scope") != "user:email" ||
		q.Get("redirect_uri") != b.e.srv.URL+signup.CallbackPath || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize URL = %q", started.URL)
	}
	return q.Get("state")
}

func (b *browser) callback(t *testing.T, who, state string) string {
	t.Helper()
	resp, err := b.client.Get(b.e.srv.URL + signup.CallbackPath + "?" + url.Values{"code": {who}, "state": {state}}.Encode())
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: %d", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

func (b *browser) setSignup(t *testing.T, st signup.Settings) {
	t.Helper()
	if status, body := b.call(t, http.MethodPut, "signup", st); status != http.StatusOK {
		t.Fatalf("set signup: %d %s", status, body)
	}
}

func (b *browser) json(t *testing.T, method, path string, body, out any) {
	t.Helper()
	status, raw := b.call(t, method, path, body)
	if status != http.StatusOK || json.Unmarshal([]byte(raw), out) != nil {
		t.Fatalf("%s %s: %d %s", method, path, status, raw)
	}
}

func (b *browser) call(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = strings.NewReader(string(data))
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, b.e.srv.URL+console.Prefix+"api/"+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func (e *env) browser(t *testing.T) *browser {
	t.Helper()
	client := &http.Client{Transport: e.srv.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	client.Jar, _ = cookiejar.New(nil)
	return &browser{e: e, client: client}
}

// newFakeGitHub answers the token, user and emails calls: a code names a person, and its token is "tok-" + the code.
func newFakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		who, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		p, known := people[who]
		switch r.URL.Path {
		case "/login/oauth/access_token":
			_ = r.ParseForm()
			if _, ok := people[r.PostForm.Get("code")]; !ok || r.PostForm.Get("client_secret") != "gh-secret" || len(r.PostForm.Get("code_verifier")) < 43 {
				_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"access_token":"tok-%s","token_type":"bearer"}`, r.PostForm.Get("code"))
		case "/user":
			if !known {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":%d,"login":%q}`, p.id, p.login)
		case "/user/emails":
			if !known {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = fmt.Fprintf(w, `[{"email":"other@example.net","primary":false,"verified":true},{"email":%q,"primary":true,"verified":%t}]`, p.email, p.verified)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type fakeBoxes struct{}

func (fakeBoxes) List(context.Context, *controlv1.ListBoxesRequest) (*controlv1.ListBoxesResponse, error) {
	return &controlv1.ListBoxesResponse{}, nil
}

func (fakeBoxes) Create(context.Context, *controlv1.CreateBoxRequest) (*controlv1.EnsureResponse, error) {
	return &controlv1.EnsureResponse{}, nil
}

func (fakeBoxes) Rename(context.Context, *controlv1.RenameBoxRequest) (*controlv1.Box, error) {
	return &controlv1.Box{}, nil
}

func (fakeBoxes) Delete(context.Context, *controlv1.BoxRef) (*controlv1.Operation, error) {
	return &controlv1.Operation{}, nil
}

func (fakeBoxes) Release(context.Context, string) error { return nil }

func (fakeBoxes) HibernateAll(context.Context, string) error { return nil }

func (fakeBoxes) DeleteState(context.Context, string) error { return nil }

type fakeGW struct {
	mu   sync.Mutex
	caps map[string]int64
}

func (*fakeGW) Add(context.Context, string) (string, string, error)              { return "gw-key", "ak", nil }
func (*fakeGW) Revoke(context.Context, string) error                             { return nil }
func (*fakeGW) SetEnabled(context.Context, string, bool) error                   { return nil }
func (*fakeGW) Usage(context.Context, string, time.Time) ([]gwkeys.Usage, error) { return nil, nil }

func (g *fakeGW) DailyCap(_ context.Context, owner string) (*int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.caps[owner]; ok {
		return &c, nil
	}
	return nil, nil
}

func (g *fakeGW) DailyCaps(context.Context) (map[string]int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]int64{}
	for k, v := range g.caps {
		out[k] = v
	}
	return out, nil
}

func (g *fakeGW) SetDailyCap(_ context.Context, owner string, micros *int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if micros == nil {
		delete(g.caps, owner)
	} else {
		g.caps[owner] = *micros
	}
	return nil
}

func (g *fakeGW) cap(owner string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.caps[owner]
}

type fakeTenants struct{}

func (fakeTenants) PutTenant(context.Context, string, string, int) error { return nil }
func (fakeTenants) DeleteTenant(context.Context, string) error           { return nil }

func masterKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return path
}

func freshDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("COCOONBOX_TEST_DSN")
	if base == "" {
		t.Skip("COCOONBOX_TEST_DSN names no PostgreSQL server")
	}
	name := "t_" + strings.ToLower(rand.Text()[:12])
	if err := exec(t, base, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _ = exec(t, base, "DROP DATABASE "+name+" WITH (FORCE)") })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse COCOONBOX_TEST_DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func query(t *testing.T, dsn, sql string, args ...any) pgx.Row {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn.QueryRow(t.Context(), sql, args...)
}

func exec(t *testing.T, dsn, sql string, args ...any) error {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	_, err = conn.Exec(context.Background(), sql, args...)
	return err
}
