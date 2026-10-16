package oauth_test

import (
	"crypto/rand"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/loomhand/cocoonbox/api/oauth"
	"github.com/loomhand/cocoonbox/api/users"
)

func TestCLISignsInWithADeviceTokenThatOutlivesThePersonalToken(t *testing.T) {
	e := newEnv(t)
	verifier := rand.Text() + rand.Text()
	q := accountQuery(oauth.CLIClient, verifier)
	var consent oauth.Consent
	e.console(t, http.MethodPost, "oauth/check", map[string]any{"query": q.Encode()}, &consent)
	if !consent.Account || consent.ClientID != oauth.CLIClient || consent.ClientName != "cocoonbox CLI" || consent.DeviceName != "alice-mbp" ||
		!consent.Loopback || consent.Existing {
		t.Fatalf("consent = %+v", consent)
	}
	token := e.redeemAccount(t, q, verifier)
	if user, err := e.authn.Authenticate(t.Context(), "Bearer "+token); err != nil || user != e.alice {
		t.Fatalf("the device token authenticates as %q, %v", user, err)
	}
	var devices []users.Device
	e.console(t, http.MethodGet, "devices", nil, &devices)
	if len(devices) != 1 || devices[0].Client != oauth.CLIClient || devices[0].Label != "alice-mbp" {
		t.Fatalf("devices = %+v", devices)
	}
	e.console(t, http.MethodPost, "me/token", map[string]any{}, nil)
	if user, err := e.authn.Authenticate(t.Context(), "Bearer "+token); err != nil || user != e.alice {
		t.Errorf("after the personal token's regenerate the device token authenticates as %q, %v", user, err)
	}
	if user, err := e.authn.Authenticate(t.Context(), "Bearer "+e.userToken); err == nil {
		t.Errorf("the replaced personal token still authenticates as %q", user)
	}
	if resp := e.post(t, oauth.RevokePath, "application/x-www-form-urlencoded", url.Values{"token": {token}, "client_id": {oauth.CLIClient}}.Encode()); resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if _, err := e.authn.Authenticate(t.Context(), "Bearer "+token); err == nil {
		t.Error("a revoked device token still authenticates")
	}
	e.console(t, http.MethodGet, "devices", nil, &devices)
	if len(devices) != 0 {
		t.Errorf("devices after the revoke = %+v", devices)
	}
}

func TestEachFirstPartySignInIsItsOwnDeviceAndTheConsoleRevokesOne(t *testing.T) {
	e := newEnv(t)
	cliVerifier, appVerifier := rand.Text()+rand.Text(), rand.Text()+rand.Text()
	cli := e.redeemAccount(t, accountQuery(oauth.CLIClient, cliVerifier), cliVerifier)
	app := e.redeemAccount(t, accountQuery(oauth.AppClient, appVerifier), appVerifier)
	var devices []users.Device
	e.console(t, http.MethodGet, "devices", nil, &devices)
	if len(devices) != 2 || devices[0].Client != oauth.AppClient || devices[1].Client != oauth.CLIClient {
		t.Fatalf("devices = %+v, want the app's then the CLI's", devices)
	}
	if status, _ := e.consoleStatus(t, http.MethodDelete, "devices/"+devices[1].ID, map[string]any{}); status != http.StatusNoContent {
		t.Fatalf("revoke the CLI's device: %d", status)
	}
	if _, err := e.authn.Authenticate(t.Context(), "Bearer "+cli); err == nil {
		t.Error("the CLI's revoked device token still authenticates")
	}
	if user, err := e.authn.Authenticate(t.Context(), "Bearer "+app); err != nil || user != e.alice {
		t.Errorf("the app's device token authenticates as %q, %v after the CLI's revoke", user, err)
	}
	if status, _ := e.consoleStatus(t, http.MethodDelete, "devices/"+devices[1].ID, map[string]any{}); status != http.StatusNotFound {
		t.Errorf("revoke again: %d, want 404", status)
	}
}

func TestAccountScopeIsOnlyForFirstPartyClients(t *testing.T) {
	e := newEnv(t)
	agent := e.register(t, "Agent")
	verifier := strings.Repeat("v", 43)
	tests := []struct {
		name     string
		mutate   func(q url.Values)
		redirect bool
	}{
		{"an agent asks for the account", func(q url.Values) { q.Set("client_id", agent); q.Set("redirect_uri", callback) }, true},
		{"the CLI asks for boxes", func(q url.Values) { q.Set("scope", oauth.Scope) }, true},
		{"the CLI asks for nothing", func(q url.Values) { q.Del("scope") }, true},
		{"the CLI off loopback", func(q url.Values) { q.Set("redirect_uri", "https://evil.example/callback") }, false},
		{"the CLI on another path", func(q url.Values) { q.Set("redirect_uri", "http://127.0.0.1:50123/other") }, false},
		{"a long device name", func(q url.Values) { q.Set("device_name", strings.Repeat("m", 65)) }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := accountQuery(oauth.CLIClient, verifier)
			tt.mutate(q)
			for _, call := range []string{"oauth/check", "oauth/approve"} {
				status, body := e.consoleStatus(t, http.MethodPost, call, map[string]any{"query": q.Encode()})
				var refusal struct {
					Redirect string `json:"redirect"`
				}
				_ = json.Unmarshal([]byte(body), &refusal)
				if status != http.StatusBadRequest || (refusal.Redirect != "") != tt.redirect {
					t.Errorf("%s: %d %s, want 400 with redirect %t", call, status, body, tt.redirect)
				}
			}
		})
	}
}

func TestAFirstPartyCodeOfADisabledUserGetsNoDeviceToken(t *testing.T) {
	e := newEnv(t)
	verifier := rand.Text() + rand.Text()
	q := accountQuery(oauth.CLIClient, verifier)
	code := e.approveAccount(t, q)
	if _, err := e.users.Update(t.Context(), e.alice, users.Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {oauth.CLIClient}, "redirect_uri": {q.Get("redirect_uri")}, "code_verifier": {verifier}}
	if status, body := e.tokenError(t, form); status != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
		t.Errorf("%d %s, want invalid_grant", status, body)
	}
}

func accountQuery(clientID, verifier string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"http://127.0.0.1:50123/callback"}, "code_challenge": {challenge(verifier)},
		"code_challenge_method": {"S256"}, "state": {"st"}, "scope": {oauth.AccountScope}, "device_name": {"alice-mbp"},
	}
}

func (e *env) approveAccount(t *testing.T, q url.Values) string {
	t.Helper()
	var approved struct {
		Redirect string `json:"redirect"`
	}
	e.console(t, http.MethodPost, "oauth/approve", map[string]any{"query": q.Encode()}, &approved)
	u, err := url.Parse(approved.Redirect)
	if err != nil || !strings.HasPrefix(approved.Redirect, q.Get("redirect_uri")+"?") || u.Query().Get("state") != "st" || u.Query().Get("iss") != e.srv.URL || u.Query().Get("code") == "" {
		t.Fatalf("redirect = %q", approved.Redirect)
	}
	return u.Query().Get("code")
}

func (e *env) redeemAccount(t *testing.T, q url.Values, verifier string) string {
	t.Helper()
	code := e.approveAccount(t, q)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {q.Get("client_id")}, "redirect_uri": {q.Get("redirect_uri")}, "code_verifier": {verifier}}
	resp := e.post(t, oauth.TokenPath, "application/x-www-form-urlencoded", form.Encode())
	var tok map[string]any
	if resp.StatusCode != http.StatusOK || json.UnmarshalRead(resp.Body, &tok) != nil {
		t.Fatalf("token: %d", resp.StatusCode)
	}
	token, _ := tok["access_token"].(string)
	if !strings.HasPrefix(token, users.TokenPrefix) || tok["token_type"] != "Bearer" || tok["scope"] != oauth.AccountScope || tok["refresh_token"] != nil || tok["expires_in"] != nil {
		t.Fatalf("token answer = %v, want a device token with no expiry or refresh token", tok)
	}
	return token
}
