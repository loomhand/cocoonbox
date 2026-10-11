package connect

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

const (
	defaultActivity = 50
	maxActivity     = 500
)

var policies = map[controlv1.Policy]string{
	controlv1.Policy_POLICY_UNSPECIFIED: "",
	controlv1.Policy_POLICY_ALLOW:       policyAllow,
	controlv1.Policy_POLICY_ASK:         policyAsk,
	controlv1.Policy_POLICY_OFF:         policyOff,
}

// PendingApproval is an approval that waits for the user.
type PendingApproval struct {
	ID        string `json:"id"`
	Connector string `json:"connector"`
	Tool      string `json:"tool"`
	Summary   string `json:"summary"`
	ExpiresAt int64  `json:"expires_at_ms"`
}

func (s *Service) Catalog(context.Context, *controlv1.CatalogRequest) (*controlv1.CatalogResponse, error) {
	out := &controlv1.CatalogResponse{}
	for _, e := range s.catalog {
		if e.preregistered && !e.available() {
			continue
		}
		out.Entries = append(out.Entries, &controlv1.CatalogEntry{
			Id: e.id, Name: e.name, Description: e.description, Available: e.available(), InstallUrl: e.installURL,
			GrantOnly: e.auth != authCustom && e.mcpURL == "", Checkable: e.userInfoURL != "", McpUrl: e.mcpURL, Auth: string(e.auth), IconUrl: e.icon,
		})
	}
	return out, nil
}

func (s *Service) List(ctx context.Context, req *controlv1.ListConnectorsRequest) (*controlv1.ListConnectorsResponse, error) {
	user, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	grants, err := s.store.grants(ctx, user)
	if err != nil {
		return nil, err
	}
	out := &controlv1.ListConnectorsResponse{}
	for _, g := range grants {
		out.Connectors = append(out.Connectors, s.connector(ctx, g, req.GetWithTools()))
	}
	return out, nil
}

func (s *Service) Begin(ctx context.Context, req *controlv1.BeginConnectRequest) (*controlv1.BeginConnectResponse, error) {
	if _, err := callerID(ctx); err != nil {
		return nil, err
	}
	e := s.entry(req.GetConnectorId())
	if e == nil {
		return nil, status.Errorf(codes.InvalidArgument, "unknown connector %q", req.GetConnectorId())
	}
	return &controlv1.BeginConnectResponse{Url: s.publicURL + ConsolePath + "?connect=" + url.QueryEscape(e.id)}, nil
}

func (s *Service) Disconnect(ctx context.Context, ref *controlv1.ConnectorRef) (*controlv1.DisconnectResponse, error) {
	user, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	revoked, err := s.Remove(ctx, user, ref.GetConnectorId(), DoorApp)
	return &controlv1.DisconnectResponse{Revoked: revoked}, err
}

func (s *Service) SetPolicy(ctx context.Context, req *controlv1.SetPolicyRequest) (*controlv1.Connector, error) {
	user, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	policy, ok := policies[req.GetPolicy()]
	if !ok || !toolNamePattern.MatchString(req.GetTool()) {
		return nil, status.Error(codes.InvalidArgument, "a policy names a tool and one of allow, ask, off or the default")
	}
	if err = s.store.setPolicy(ctx, user, req.GetConnectorId(), req.GetTool(), policy); err != nil {
		return nil, grantStatus(err)
	}
	s.watchers.notify(user)
	g, err := s.store.grant(ctx, user, req.GetConnectorId())
	if err != nil {
		return nil, grantStatus(err)
	}
	return s.connector(ctx, g, true), nil
}

func (s *Service) Approve(ctx context.Context, req *controlv1.ApproveRequest) (*controlv1.ApproveResponse, error) {
	user, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	decision := map[controlv1.ApprovalDecision]string{
		controlv1.ApprovalDecision_APPROVAL_DECISION_ONCE:   decisionOnce,
		controlv1.ApprovalDecision_APPROVAL_DECISION_ALWAYS: decisionAlways,
		controlv1.ApprovalDecision_APPROVAL_DECISION_DENY:   decisionDeny,
	}[req.GetDecision()]
	if decision == "" {
		return nil, status.Error(codes.InvalidArgument, "a decision is once, always or deny")
	}
	a, err := s.store.decide(ctx, user, req.GetApprovalId(), decision)
	if errors.Is(err, errNoApproval) {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return nil, err
	}
	if decision == decisionAlways {
		if err := s.store.setPolicy(ctx, user, a.connector, a.tool, policyAllow); err != nil {
			return nil, grantStatus(err)
		}
		s.watchers.notify(user)
	}
	log.WithFunc("connect.Service.Approve").Infof(ctx, "user %s answered approval %s (%s %s): %s", user, a.id, a.connector, a.tool, decision)
	return &controlv1.ApproveResponse{}, nil
}

func (s *Service) Activity(ctx context.Context, req *controlv1.ActivityRequest) (*controlv1.ActivityResponse, error) {
	user, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultActivity
	}
	records, err := s.store.activity(ctx, user, req.GetConnectorId(), min(limit, maxActivity))
	if err != nil {
		return nil, err
	}
	out := &controlv1.ActivityResponse{}
	for _, r := range records {
		out.Entries = append(out.Entries, &controlv1.ActivityEntry{
			AtMs: r.at.UnixMilli(), ConnectorId: r.connector, Tool: r.tool, Door: r.door, Caller: r.caller, Decision: r.decision,
			UpstreamStatus: int32(r.status), //nolint:gosec // an HTTP status LatencyMs: r.latency.Milliseconds(), RequestBytes: r.reqBytes, ResponseBytes: r.respBytes,
			ArgsHash:       r.argsSHA256, Outcome: r.outcome,
		})
	}
	return out, nil
}

func (s *Service) SpendRequests(ctx context.Context, _ *controlv1.SpendRequestsRequest) (*controlv1.SpendRequestsResponse, error) {
	user, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	switch _, err := s.linkGrant(ctx, user); {
	case errors.Is(err, errNoGrant):
		return &controlv1.SpendRequestsResponse{}, nil
	case err != nil:
		return nil, err
	}
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	p := s.spends[user]
	if p == nil || p.polledAt.IsZero() {
		return &controlv1.SpendRequestsResponse{}, nil
	}
	return &controlv1.SpendRequestsResponse{Requests: p.requests, PolledAtMs: p.polledAt.UnixMilli()}, nil
}

// Remove revokes user's grant of connector at the vendor where it can, deletes it, and reports whether the vendor revoked it.
func (s *Service) Remove(ctx context.Context, user, connector, door string) (bool, error) {
	g, err := s.store.grant(ctx, user, connector)
	if err != nil {
		return false, grantStatus(err)
	}
	logger := log.WithFunc("connect.Service.Remove")
	revoked := s.revokeGrant(ctx, g)
	if err := s.store.deleteGrant(ctx, g.id); err != nil {
		return false, err
	}
	s.dropConn(g.id)
	s.changed(ctx, g)
	if err := s.store.audit(ctx, &record{user: user, connector: connector, door: door, caller: door, decision: decisionDisconn, outcome: outcomeOK}); err != nil {
		logger.Error(ctx, err, "write the audit record")
	}
	logger.Infof(ctx, "user %s disconnected %s (grant %s), revoked at the vendor: %t", user, connector, g.id, revoked)
	return revoked, nil
}

// RevokeAll revokes user's grants at the vendors where it can; the grants go with the user's row.
func (s *Service) RevokeAll(ctx context.Context, user string) error {
	grants, err := s.store.grants(ctx, user)
	if err != nil {
		return err
	}
	for _, g := range grants {
		s.revokeGrant(ctx, g)
		s.dropConn(g.id)
	}
	return nil
}

// Pending lists user's unanswered approvals, the oldest first.
func (s *Service) Pending(ctx context.Context, user string) ([]PendingApproval, error) {
	list, err := s.store.pending(ctx, user)
	if err != nil {
		return nil, err
	}
	out := make([]PendingApproval, 0, len(list))
	for _, a := range list {
		out = append(out, PendingApproval{ID: a.id, Connector: a.connector, Tool: a.tool, Summary: a.summary, ExpiresAt: a.expiresAt.UnixMilli()})
	}
	return out, nil
}

// Check reads user's account of connector from the vendor's user endpoint, refreshing the grant when it is near expiry,
// and once more after a 401.
func (s *Service) Check(ctx context.Context, user, connector string) (string, error) {
	g, err := s.store.grant(ctx, user, connector)
	if err != nil {
		return "", grantStatus(err)
	}
	e := s.entry(g.kind)
	if e == nil || e.userInfoURL == "" || g.status != statusActive {
		return "", status.Errorf(codes.FailedPrecondition, "%s cannot be checked; reconnect it if it needs a reconnect", connector)
	}
	sec, err := s.secrets(ctx, g)
	if err != nil {
		return "", err
	}
	access, err := s.access(ctx, g, sec.AccessToken, false)
	if err != nil {
		return "", checkStatus(err)
	}
	label, code := s.accountLabel(ctx, e, access)
	if code == http.StatusUnauthorized && sec.RefreshToken != "" {
		if access, err = s.access(ctx, g, access, true); err != nil {
			return "", checkStatus(err)
		}
		label, code = s.accountLabel(ctx, e, access)
	}
	switch {
	case code == http.StatusUnauthorized:
		return "", checkStatus(s.lose(ctx, g))
	case code != http.StatusOK:
		return "", status.Errorf(codes.Unavailable, "%s answered %d", e.name, code)
	}
	return label, nil
}

func (s *Service) revokeGrant(ctx context.Context, g *grant) bool {
	logger := log.WithFunc("connect.Service.revokeGrant")
	sec, err := s.secrets(ctx, g)
	if err != nil {
		logger.Warnf(ctx, "open grant %s for its revoke: %v", g.id, err)
		return false
	}
	revoked, err := s.revokeAtVendor(ctx, g, sec)
	if err != nil {
		logger.Warnf(ctx, "revoke grant %s of %s (%s) at the vendor: %v", g.id, g.user, g.connector, err)
	}
	return revoked
}

// revokeAtVendor refreshes an expiring GitHub token first, since GitHub revokes by a live access token.
func (s *Service) revokeAtVendor(ctx context.Context, g *grant, sec *secrets) (bool, error) {
	if sec.AccessToken == "" {
		return false, nil
	}
	e := s.entry(g.kind)
	if e != nil && e.auth == authOAuth {
		if e.revoke == revokeGitHub {
			if access, err := s.access(ctx, g, sec.AccessToken, false); err == nil {
				sec.AccessToken = access
			}
		}
		return s.revoke(ctx, e.revoke, e.client, *sec)
	}
	c, err := s.client(g, *sec)
	if err != nil {
		return false, err
	}
	return s.revoke(ctx, revokeRFC7009, c, *sec)
}

func (s *Service) connector(ctx context.Context, g *grant, withTools bool) *controlv1.Connector {
	out := &controlv1.Connector{
		Id: g.connector, Kind: g.kind, Name: s.vendorName(g), Account: g.account, Status: g.status, Scopes: g.scopes,
		McpUrl: g.mcpURL, ConnectedAtMs: g.createdAt.UnixMilli(),
	}
	if !withTools || g.status != statusActive {
		return out
	}
	tools, err := s.tools(ctx, g)
	if err != nil {
		log.WithFunc("connect.Service.connector").Warnf(ctx, "list the tools of %s for %s: %v", g.connector, g.user, err)
		out.ToolsError = shorten(err.Error(), maxErrorExcerpt)
		return out
	}
	for _, t := range tools {
		out.Tools = append(out.Tools, &controlv1.ConnectorTool{
			Name: t.Name, Title: t.Title, Description: t.Description, ReadOnly: t.ReadOnly, Destructive: t.Destructive,
			Policy: policyEnum(g.policy[t.Name]), Effective: policyEnum(t.effective(g.policy[t.Name])),
		})
	}
	return out
}

func callerID(ctx context.Context) (string, error) {
	user := auth.User(ctx)
	if user == "" {
		return "", status.Error(codes.Unauthenticated, "missing user")
	}
	return user, nil
}

func grantStatus(err error) error {
	if errors.Is(err, errNoGrant) {
		return status.Error(codes.NotFound, err.Error())
	}
	return err
}

func checkStatus(err error) error {
	if errors.Is(err, errReconnect) {
		return status.Error(codes.FailedPrecondition, "the vendor no longer accepts the grant; reconnect it")
	}
	return err
}

func policyEnum(policy string) controlv1.Policy {
	for k, v := range policies {
		if v == policy {
			return k
		}
	}
	return controlv1.Policy_POLICY_UNSPECIFIED
}
