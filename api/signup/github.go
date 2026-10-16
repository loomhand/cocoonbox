package signup

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	githubAuthorize = "https://github.com/login/oauth/authorize"
	githubToken     = "https://github.com/login/oauth/access_token" //nolint:gosec // a URL, not a credential
	githubAPI       = "https://api.github.com"
	maxAnswer       = 1 << 20
)

// identity is a GitHub account as sign-in knows it: the stable numeric id, the login and the verified primary email.
type identity struct {
	subject  string
	login    string
	email    string
	verified bool
}

// GitHubClient is the GitHub OAuth App that signs people in, which asks for no scope but user:email; empty URLs take GitHub's own.
type GitHubClient struct {
	ClientID     string
	ClientSecret string
	AuthorizeURL string
	TokenURL     string
	APIURL       string
}

func (c GitHubClient) configured() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

func (c GitHubClient) authorize(state, challenge, redirect string) string {
	q := url.Values{
		"client_id": {c.ClientID}, "redirect_uri": {redirect}, "scope": {"user:email"}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	return cmp.Or(c.AuthorizeURL, githubAuthorize) + "?" + q.Encode()
}

// identity redeems code for an access token, reads the account with it and drops it: sign-in keeps no GitHub token.
func (c GitHubClient) identity(ctx context.Context, client *http.Client, code, verifier, redirect string) (identity, error) {
	form := url.Values{"client_id": {c.ClientID}, "client_secret": {c.ClientSecret}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cmp.Or(c.TokenURL, githubToken), strings.NewReader(form.Encode()))
	if err != nil {
		return identity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var token struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := send(client, req, &token); err != nil {
		return identity{}, fmt.Errorf("redeem the GitHub code: %w", err)
	}
	if token.AccessToken == "" {
		return identity{}, fmt.Errorf("redeem the GitHub code: %s %s", token.Error, token.Description)
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := c.get(ctx, client, token.AccessToken, "/user", &user); err != nil {
		return identity{}, err
	}
	if user.ID == 0 {
		return identity{}, errors.New("GitHub answered no account id")
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := c.get(ctx, client, token.AccessToken, "/user/emails", &emails); err != nil {
		return identity{}, err
	}
	id := identity{subject: strconv.FormatInt(user.ID, 10), login: user.Login}
	for _, e := range emails {
		if e.Primary {
			id.email, id.verified = strings.ToLower(e.Email), e.Verified
		}
	}
	return id, nil
}

func (c GitHubClient) get(ctx context.Context, client *http.Client, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cmp.Or(c.APIURL, githubAPI)+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if err := send(client, req, out); err != nil {
		return fmt.Errorf("read GitHub %s: %w", path, err)
	}
	return nil
}

func send(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.UnmarshalRead(io.LimitReader(resp.Body, maxAnswer), out)
}
