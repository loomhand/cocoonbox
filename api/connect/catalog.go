package connect

import "net/url"

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
	}
	if github.Slug != "" {
		gh.installURL = "https://github.com/apps/" + url.PathEscape(github.Slug) + "/installations/new"
	}
	return []*entry{
		gh,
		{
			id: "google-calendar", name: "Google Calendar", description: "Your Google Calendar events. Needs Google's Workspace MCP preview for the account.",
			mcpURL: "https://calendarmcp.googleapis.com/mcp/v1", auth: authOAuth,
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
