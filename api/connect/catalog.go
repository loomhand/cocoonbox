package connect

import (
	"encoding/base64"
	"net/url"
)

const (
	authOAuth  authKind = "oauth"
	authMCP    authKind = "mcp"
	authCustom authKind = "custom"
	authOpen   authKind = "none"

	revokeGitHub  revokeKind = "github"
	revokeRFC7009 revokeKind = "rfc7009"

	kindCustom = "custom"

	linkID          = "link"
	linkLogin       = "https://login.link.com"
	linkAPI         = "https://api.link.com"
	linkHost        = "api.link.com"
	linkPlaceholder = "cbc_link"
)

// slackScopes are the user scopes of every tool of Slack's MCP server; the Slack app must list the same.
var slackScopes = []string{
	"search:read.public", "search:read.private", "search:read.mpim", "search:read.im", "search:read.files", "search:read.users",
	"channels:history", "groups:history", "mpim:history", "im:history",
	"channels:read", "groups:read", "im:read", "mpim:read",
	"channels:write", "groups:write", "im:write", "mpim:write",
	"chat:write", "reactions:read", "reactions:write", "files:read", "files:write", "emoji:read",
	"canvases:read", "canvases:write", "lists:read", "lists:write", "users:read", "users:read.email",
}

// Credentials is an OAuth client the api registered with a vendor; a GitHub App adds its slug, Link the Stripe publishable key.
type Credentials struct {
	ClientID     string
	ClientSecret string
	Slug         string
	Key          string
}

type authKind string

type revokeKind string

// render turns an access token into the value a header carries.
type render func(token string) string

// injection is a claim credential a grant puts on its owner's boxes: sandboxd writes value into header on requests to hosts that carry placeholder.
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
	icon        string

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
	// guest are guestEnv records, NAME=placeholder;hosts;how as in the vault, the image renders into box shells while the grant injects.
	guest []string
	// preregistered marks a vendor that takes only the api's own client, set up by hand; the catalog leaves it out until it is set.
	preregistered bool
}

func (e *entry) available() bool {
	return (e.auth != authOAuth && !e.preregistered) || (e.client.ID != "" && e.client.Secret != "")
}

func newCatalog(opts Options) []*entry {
	gh := &entry{
		id: "github", name: "GitHub", description: "Issues, pull requests and code in the repositories you pick for the CocoonBox GitHub App.",
		mcpURL: "https://api.githubcopilot.com/mcp/", auth: authOAuth, icon: "https://github.githubassets.com/favicons/favicon.svg",
		client: client{ //nolint:gosec // the secret comes from the environment
			ID: opts.GitHub.ClientID, Secret: opts.GitHub.ClientSecret, TokenURL: "https://github.com/login/oauth/access_token",
			RevokeURL: "https://api.github.com/applications/" + url.PathEscape(opts.GitHub.ClientID) + "/grant",
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
	if opts.Link.Key == "" {
		opts.Link = Credentials{}
	}
	google := client{ //nolint:gosec // the secret comes from the environment
		ID: opts.Google.ClientID, Secret: opts.Google.ClientSecret, TokenURL: "https://oauth2.googleapis.com/token",
		RevokeURL: "https://oauth2.googleapis.com/revoke",
	}
	calendar := googleEntry(google, "google-calendar", "Google Calendar", "Your Google Calendar events, for programs in your box. It serves no tools to agents yet.",
		"https://calendar.google.com/googlecalendar/images/favicons_2020q4/calendar_31.ico", "https://www.googleapis.com/auth/calendar.events")
	calendar.inject = []injection{{
		name: "GOOGLE_CALENDAR", hosts: []string{"www.googleapis.com"}, header: headerAuthorization,
		placeholder: "Bearer @GOOGLE_CALENDAR@", value: prefixed("Bearer "),
	}}
	tasks := googleEntry(google, "google-tasks", "Google Tasks", "Your Google Tasks lists and tasks.",
		"https://www.gstatic.com/images/branding/product/1x/tasks_48dp.png", "https://www.googleapis.com/auth/tasks")
	tasks.mcpURL = builtinGoogleTasks
	if opts.GitHub.Slug != "" {
		gh.installURL = "https://github.com/apps/" + url.PathEscape(opts.GitHub.Slug) + "/installations/new"
	}
	return []*entry{
		gh,
		calendar,
		tasks,
		{id: "notion", name: "Notion", description: "Pages and databases in your Notion workspace.", mcpURL: "https://mcp.notion.com/mcp", auth: authMCP, icon: "https://www.notion.so/images/favicon.ico"},
		{id: "linear", name: "Linear", description: "Issues and projects in your Linear workspace.", mcpURL: "https://mcp.linear.app/mcp", auth: authMCP, icon: "https://linear.app/favicon.ico"},
		{id: "attio", name: "Attio", description: "Records, lists and notes in your Attio workspace.", mcpURL: "https://mcp.attio.com/mcp", auth: authMCP, icon: "https://attio.com/favicon.ico"},
		{id: "monday", name: "monday.com", description: "Boards, items and updates in your monday.com account.", mcpURL: "https://mcp.monday.com/mcp", auth: authMCP, icon: "https://monday.com/favicon.ico"},
		{id: "atlassian", name: "Jira & Confluence", description: "Jira issues and Confluence pages in your Atlassian sites.", mcpURL: "https://mcp.atlassian.com/v1/mcp", auth: authMCP, icon: "https://wac-cdn.atlassian.com/assets/img/favicons/atlassian/favicon.png"},
		{id: "airtable", name: "Airtable", description: "Bases, tables and records in your Airtable workspaces.", mcpURL: "https://mcp.airtable.com/mcp", auth: authMCP, icon: "https://airtable.com/favicon.ico"},
		{id: "granola", name: "Granola", description: "Your Granola meeting notes and transcripts.", mcpURL: "https://mcp.granola.ai/mcp", auth: authMCP, icon: "https://www.granola.ai/favicon/favicon-96x96.png"},
		{id: "webflow", name: "Webflow", description: "Sites, pages and CMS collections in your Webflow workspace.", mcpURL: "https://mcp.webflow.com/mcp", auth: authMCP, icon: "https://webflow.com/favicon.ico"},
		{id: "mintlify", name: "Mintlify", description: "Documentation sites in your Mintlify account.", mcpURL: "https://mcp.mintlify.com/mcp", auth: authMCP, icon: "https://www.mintlify.com/favicon.ico"},
		{id: "sentry", name: "Sentry", description: "Issues, errors and projects in your Sentry organization.", mcpURL: "https://mcp.sentry.dev/mcp", auth: authMCP, icon: "https://sentry.io/favicon.ico"},
		{id: "supabase", name: "Supabase", description: "Projects, databases and functions in your Supabase organization.", mcpURL: "https://mcp.supabase.com/mcp", auth: authMCP, icon: "https://supabase.com/favicon/favicon.ico"},
		{id: "cloudflare", name: "Cloudflare", description: "Workers, KV, R2 and D1 in your Cloudflare account.", mcpURL: "https://bindings.mcp.cloudflare.com/mcp", auth: authMCP, icon: "https://www.cloudflare.com/favicon.ico"},
		{id: "posthog", name: "PostHog", description: "Product analytics, feature flags and insights in your PostHog project.", mcpURL: "https://mcp.posthog.com/mcp", auth: authMCP, icon: "https://posthog.com/favicon-32x32.png"},
		{id: "stripe", name: "Stripe", description: "Customers, payments and invoices in your Stripe account.", mcpURL: "https://mcp.stripe.com", auth: authMCP, icon: "https://stripe.com/favicon.ico"},
		{id: "paypal", name: "PayPal", description: "Invoices, orders and transactions in your PayPal business account.", mcpURL: "https://mcp.paypal.com/mcp", auth: authMCP, icon: "https://www.paypal.com/favicon.ico"},
		{id: "zapier", name: "Zapier", description: "The actions you enable in your Zapier MCP server.", mcpURL: "https://mcp.zapier.com/api/mcp/mcp", auth: authMCP, icon: "https://zapier.com/favicon.ico"},
		{id: "composio", name: "Composio", description: "The apps and tools you connect in Composio.", mcpURL: "https://connect.composio.dev/mcp", auth: authMCP, icon: "https://composio.dev/favicon.ico"},
		{
			id: "hubspot", name: "HubSpot", description: "Contacts, companies, deals and tickets in your HubSpot account.", mcpURL: "https://mcp.hubspot.com",
			auth: authMCP, icon: "https://www.hubspot.com/favicon.ico", client: client{ID: opts.HubSpot.ClientID, Secret: opts.HubSpot.ClientSecret}, preregistered: true,
		},
		{
			id: "slack", name: "Slack", description: "Messages, channels, canvases and people in your Slack workspace.", mcpURL: "https://mcp.slack.com/mcp",
			auth: authMCP, icon: "https://slack.com/favicon.ico", client: client{ID: opts.Slack.ClientID, Secret: opts.Slack.ClientSecret}, preregistered: true,
			scopes: slackScopes,
		},
		{
			id: linkID, name: "Link", description: "Your Link wallet, for programs in your box to pay with one-time cards you approve in the Link app.",
			auth: authOAuth, icon: "https://images.stripeassets.com/fzn2n1nzq965/35yClx6UuTze6h851WsJqW/18da88ac03bd96140d16bdac62b94578/app-icon.png",
			client: client{
				ID: opts.Link.ClientID, Secret: opts.Link.ClientSecret, Key: opts.Link.Key, TokenURL: linkLogin + "/auth/token", RevokeURL: linkLogin + "/auth/revoke",
			},
			authorizeURL: linkLogin + "/auth",
			scopes:       []string{"payment_methods.agentic", "userinfo:read"},
			params:       url.Values{"key": {opts.Link.Key}},
			userInfoURL:  linkAPI + "/userinfo", userInfoName: "email",
			revoke: revokeRFC7009,
			inject: []injection{{
				name: "LINK_API", hosts: []string{linkHost}, header: headerAuthorization, placeholder: "Bearer " + linkPlaceholder, value: prefixed("Bearer "),
			}},
			guest:         []string{"LINK_ACCESS_TOKEN=" + linkPlaceholder + ";" + linkHost + ";Authorization:%20Bearer%20{}", "LINK_NO_REFRESH=1;;"},
			preregistered: true,
		},
		{id: "exa", name: "Exa", description: "Web search and page contents from Exa, with no sign-in.", mcpURL: "https://mcp.exa.ai/mcp", auth: authOpen, icon: "https://exa.ai/images/favicon-32x32.png"},
		{
			id: "context7", name: "Context7", description: "Current documentation and code examples for libraries, with no sign-in.", mcpURL: "https://mcp.context7.com/mcp",
			auth: authOpen, icon: "https://context7.com/context7-icon-green.png",
		},
		{id: kindCustom, name: "Custom MCP server", description: "Any MCP server by URL, with its own sign-in, a key in a header, or no sign-in.", auth: authCustom},
	}
}

func googleEntry(google client, id, name, description, icon, scope string) *entry {
	return &entry{
		id: id, name: name, description: description, auth: authOAuth, icon: icon,
		client:       google,
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		scopes:       []string{"openid", "https://www.googleapis.com/auth/userinfo.email", scope},
		params:       url.Values{"access_type": {"offline"}, "prompt": {"consent"}, "include_granted_scopes": {"true"}},
		userInfoURL:  "https://openidconnect.googleapis.com/v1/userinfo", userInfoName: "email",
		revoke: revokeRFC7009,
	}
}

func gitBasic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

func prefixed(scheme string) render {
	return func(token string) string { return scheme + token }
}
