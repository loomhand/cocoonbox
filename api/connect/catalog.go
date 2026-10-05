package connect

import (
	"encoding/base64"
	"net/url"
)

const (
	authOAuth  authKind = "oauth"
	authMCP    authKind = "mcp"
	authCustom authKind = "custom"

	revokeGitHub  revokeKind = "github"
	revokeRFC7009 revokeKind = "rfc7009"

	kindCustom = "custom"
)

// Credentials is an OAuth client the api registered with a vendor; a GitHub App adds its slug.
type Credentials struct {
	ClientID     string
	ClientSecret string
	Slug         string
}

type authKind string

type revokeKind string

// render turns an access token into the value a header carries.
type render func(token string) string

// injection is a claim credential a grant puts on its owner's boxes (M5.2): sandboxd writes value(access token) into
// header on requests to hosts that carry placeholder.
type injection struct {
	name        string
	hosts       []string
	header      string
	placeholder string
	value       render
}

type entry struct {
	id          string
	name        string
	description string
	mcpURL      string
	auth        authKind

	client       client
	authorizeURL string
	scopes       []string
	params       url.Values
	userInfoURL  string
	userInfoName string
	revoke       revokeKind
	installURL   string
	inject       []injection
	// flag is the guest-visible switch the image follows while the grant injects.
	flag string
}

func (e *entry) available() bool {
	return e.auth != authOAuth || (e.client.ID != "" && e.client.Secret != "")
}

func newCatalog(github, google Credentials) []*entry {
	gh := &entry{
		id: "github", name: "GitHub", description: "Issues, pull requests and code in the repositories you pick for the CocoonBox GitHub App.",
		mcpURL: "https://api.githubcopilot.com/mcp/", auth: authOAuth,
		client: client{ //nolint:gosec // the secret comes from the environment
			ID: github.ClientID, Secret: github.ClientSecret, TokenURL: "https://github.com/login/oauth/access_token",
			RevokeURL: "https://api.github.com/applications/" + url.PathEscape(github.ClientID) + "/grant",
		},
		authorizeURL: "https://github.com/login/oauth/authorize",
		userInfoURL:  "https://api.github.com/user", userInfoName: "login",
		revoke: revokeGitHub,
		inject: []injection{
			{name: "GH_GIT", hosts: []string{"github.com"}, header: headerAuthorization, placeholder: "Basic @GH_GIT@", value: gitBasic},
			{name: "GH_API", hosts: []string{"api.github.com"}, header: headerAuthorization, placeholder: "token @GH_API@", value: prefixed("token ")},
		},
		flag: "COCOONBOX_GH_GIT",
	}
	if github.Slug != "" {
		gh.installURL = "https://github.com/apps/" + url.PathEscape(github.Slug) + "/installations/new"
	}
	return []*entry{
		gh,
		{
			id: "google-calendar", name: "Google Calendar", description: "Your Google Calendar events, for programs in your box. It serves no tools to agents yet.",
			auth: authOAuth,
			inject: []injection{{
				name: "GOOGLE_CALENDAR", hosts: []string{"www.googleapis.com"}, header: headerAuthorization,
				placeholder: "Bearer @GOOGLE_CALENDAR@", value: prefixed("Bearer "),
			}},
			client: client{ //nolint:gosec // the secret comes from the environment
				ID: google.ClientID, Secret: google.ClientSecret, TokenURL: "https://oauth2.googleapis.com/token",
				RevokeURL: "https://oauth2.googleapis.com/revoke",
			},
			authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
			scopes:       []string{"openid", "https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/calendar.events"},
			params:       url.Values{"access_type": {"offline"}, "prompt": {"consent"}, "include_granted_scopes": {"true"}},
			userInfoURL:  "https://openidconnect.googleapis.com/v1/userinfo", userInfoName: "email",
			revoke: revokeRFC7009,
		},
		{id: "notion", name: "Notion", description: "Pages and databases in your Notion workspace.", mcpURL: "https://mcp.notion.com/mcp", auth: authMCP},
		{id: "linear", name: "Linear", description: "Issues and projects in your Linear workspace.", mcpURL: "https://mcp.linear.app/mcp", auth: authMCP},
		{id: kindCustom, name: "Custom MCP server", description: "Any MCP server by URL, with its own sign-in or a key in a header.", auth: authCustom},
	}
}

func gitBasic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

func prefixed(scheme string) render {
	return func(token string) string { return scheme + token }
}
