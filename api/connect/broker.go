package connect

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/errgroup"
)

const (
	approvalMeta   = "cocoonbox/approval"
	approvalIDMeta = "cocoonbox/approval_id"
	summaryValue   = 80
	summaryTotal   = 400
	listFanout     = 8
)

// lostGrant are the token errors after which only a new consent helps; GitHub names its own.
var lostGrant = []string{"invalid_grant", "bad_refresh_token", "unauthorized_client"}

type tool struct {
	Name        string
	Title       string
	Description string
	ReadOnly    bool
	Destructive bool
	raw         map[string]jsontext.Value
}

func (t tool) effective(choice string) string {
	switch {
	case choice != "":
		return choice
	case t.ReadOnly:
		return policyAllow
	default:
		return policyAsk
	}
}

type caller struct {
	user string
	door string
	name string
}

type callResult struct {
	Content []textContent  `json:"content"`
	IsError bool           `json:"isError"`
	Meta    map[string]any `json:"_meta,omitzero"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolCall struct {
	Name      string         `json:"name"`
	Arguments jsontext.Value `json:"arguments"`
}

func (s *Service) doorTools(ctx context.Context, c caller) ([]map[string]jsontext.Value, error) {
	grants, err := s.store.grants(ctx, c.user)
	if err != nil {
		return nil, err
	}
	lists := make([][]map[string]jsontext.Value, len(grants))
	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(listFanout)
	for i, g := range grants {
		if g.status != statusActive || g.mcpURL == "" {
			continue
		}
		eg.Go(func() error {
			tools, err := s.tools(egCtx, g)
			if err != nil {
				log.WithFunc("connect.Service.doorTools").Warnf(ctx, "list the tools of %s for %s: %v", g.connector, g.user, err)
				return nil
			}
			for _, t := range tools {
				if exposed, ok := expose(g, t, c.door); ok {
					lists[i] = append(lists[i], exposed)
				}
			}
			return nil
		})
	}
	_ = eg.Wait()
	return slices.Concat(lists...), nil
}

// doorCall returns the upstream result without its _meta, so an upstream cannot forge an approval request.
func (s *Service) doorCall(ctx context.Context, c caller, name string, args jsontext.Value, approvalID string) (any, error) {
	connector, toolName, ok := strings.Cut(name, "_")
	if !ok || !connectorPattern.MatchString(connector) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + name}
	}
	if len(args) == 0 || string(args) == "null" {
		args = jsontext.Value("{}")
	}
	canonical := args.Clone()
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "arguments are not valid JSON"}
	}
	sum := sha256.Sum256(canonical)
	rec := &record{user: c.user, connector: connector, tool: toolName, door: c.door, caller: c.name, argsSHA256: hex.EncodeToString(sum[:])}
	start := time.Now()
	defer func() {
		rec.latency = time.Since(start)
		if err := s.store.audit(context.WithoutCancel(ctx), rec); err != nil {
			log.WithFunc("connect.Service.doorCall").Error(ctx, err, "write the audit record")
		}
	}()
	g, err := s.store.grant(ctx, c.user, connector)
	if errors.Is(err, errNoGrant) {
		rec.decision, rec.outcome = policyOff, outcomeError
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + name}
	}
	if err != nil {
		rec.outcome = outcomeError
		return nil, err
	}
	if g.status != statusActive {
		rec.decision, rec.outcome = statusReconnect, outcomeError
		return s.reconnectText(g), nil
	}
	tools, err := s.tools(ctx, g)
	if err != nil {
		rec.outcome = outcomeError
		return s.failureText(g, err), nil
	}
	i := slices.IndexFunc(tools, func(t tool) bool { return t.Name == toolName })
	if i < 0 {
		rec.decision, rec.outcome = policyOff, outcomeError
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + name}
	}
	t := tools[i]
	switch policy := t.effective(g.policy[t.Name]); {
	case policy == policyOff:
		rec.decision, rec.outcome = policyOff, outcomeError
		return errorText("The tool " + name + " is off in your CocoonBox Connect settings."), nil
	case policy == policyAsk && c.door == DoorBox && approvalID == "":
		rec.decision, rec.outcome = decisionRequired, outcomeError
		return s.requestApproval(ctx, g, t, args, rec.argsSHA256)
	case policy == policyAsk && c.door == DoorBox:
		reason, useErr := s.store.useApproval(ctx, approvalID, c.user, g.id, t.Name, rec.argsSHA256)
		if useErr != nil {
			rec.outcome = outcomeError
			return nil, useErr
		}
		if reason != "" {
			rec.decision, rec.outcome = decisionDenied, outcomeError
			return errorText("Refused: " + reason + "."), nil
		}
		rec.decision = decisionApproved
	default:
		rec.decision = policyAllow
	}
	result, ex, err := s.invoke(ctx, g, t, args)
	rec.status, rec.reqBytes, rec.respBytes = ex.status, ex.reqBytes, ex.respBytes
	switch {
	case errors.Is(err, errReconnect):
		rec.outcome = outcomeError
		return s.reconnectText(g), nil
	case err != nil && !t.ReadOnly && uncertain(err, ex):
		rec.outcome = outcomeUnknown
		return errorText(fmt.Sprintf("%s: the call to %s may or may not have taken effect (%v). It is not retried; check the result with a read tool before you call it again.",
			outcomeUnknown, s.vendorName(g), err)), nil
	case err != nil:
		rec.outcome = outcomeError
		if rpcErr, ok := errors.AsType[*rpcError](err); ok {
			return nil, rpcErr
		}
		return s.failureText(g, err), nil
	}
	var out map[string]jsontext.Value
	if err := json.Unmarshal(result, &out); err != nil {
		rec.outcome = outcomeError
		return s.failureText(g, fmt.Errorf("decode the result: %w", err)), nil
	}
	delete(out, "_meta")
	rec.outcome = outcomeOK
	if string(out["isError"]) == "true" {
		rec.outcome = outcomeError
	}
	return out, nil
}

func (s *Service) tools(ctx context.Context, g *grant) ([]tool, error) {
	if g.mcpURL == "" {
		return nil, nil
	}
	c := s.conn(g.id)
	c.mu.Lock()
	cached, at := c.tools, c.toolsAt
	c.mu.Unlock()
	if cached != nil && time.Since(at) < toolsTTL {
		return cached, nil
	}
	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	var tools []tool
	err := s.withCredential(ctx, g, func(u *upstream) error {
		var err error
		tools, err = u.listTools(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.tools, c.toolsAt = tools, time.Now()
	c.mu.Unlock()
	return tools, nil
}

// invoke resends only after a 401, which the server ran nothing for.
func (s *Service) invoke(ctx context.Context, g *grant, t tool, args jsontext.Value) (jsontext.Value, exchange, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var result jsontext.Value
	var ex exchange
	err := s.withCredential(ctx, g, func(u *upstream) error {
		var err error
		result, ex, err = u.call(ctx, "tools/call", toolCall{Name: t.Name, Arguments: args})
		return err
	})
	return result, ex, err
}

// withCredential refreshes once on a 401; a second 401 marks the grant needs_reconnect.
func (s *Service) withCredential(ctx context.Context, g *grant, fn func(*upstream) error) error {
	sec, err := s.secrets(ctx, g)
	if err != nil {
		return err
	}
	if sec.AccessToken == "" {
		if err = fn(&upstream{s: s, url: g.mcpURL, cred: credential{header: sec.Header, value: sec.APIKey}, conn: s.conn(g.id)}); errors.Is(err, errUnauthorized) {
			return s.lose(ctx, g)
		}
		return err
	}
	access, err := s.access(ctx, g, sec.AccessToken, false)
	if err != nil {
		return err
	}
	u := &upstream{s: s, url: g.mcpURL, cred: credential{header: headerAuthorization, value: "Bearer " + access}, conn: s.conn(g.id)}
	if err = fn(u); !errors.Is(err, errUnauthorized) {
		return err
	}
	if sec.RefreshToken == "" {
		return s.lose(ctx, g)
	}
	if access, err = s.access(ctx, g, access, true); err != nil {
		return err
	}
	u.cred.value = "Bearer " + access
	if err = fn(u); !errors.Is(err, errUnauthorized) {
		return err
	}
	return s.lose(ctx, g)
}

// access refreshes near expiry, or when force is set and the grant still holds seen; one refresh runs per grant.
func (s *Service) access(ctx context.Context, g *grant, seen string, force bool) (string, error) {
	if !force && (g.expiresAt == nil || time.Until(*g.expiresAt) > refreshMargin) {
		return seen, nil
	}
	v, err, _ := s.refreshes.Do(g.id, func() (any, error) {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tokenTimeout+setupTimeout)
		defer cancel()
		return s.refresh(refreshCtx, g.id, seen, force)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// refresh commits a rotated refresh token before the new access token is used.
func (s *Service) refresh(ctx context.Context, id, stale string, force bool) (string, error) {
	logger := log.WithFunc("connect.Service.refresh")
	var access string
	var rotated bool
	err := s.store.refreshGrant(ctx, id, func(g *grant) (*grant, error) {
		var sec secrets
		if err := s.seal.open(ctx, g.user, g.sealed, grantData(g.id, g.user, g.connector), &sec); err != nil {
			return nil, err
		}
		fresh := g.expiresAt == nil || time.Until(*g.expiresAt) > refreshMargin
		if fresh && (!force || sec.AccessToken != stale) {
			access = sec.AccessToken
			return nil, nil
		}
		if g.status != statusActive || sec.RefreshToken == "" {
			return nil, errReconnect
		}
		c, err := s.client(g, sec)
		if err != nil {
			return nil, err
		}
		t, err := s.token(ctx, c, url.Values{"grant_type": {grantRefreshToken}, grantRefreshToken: {sec.RefreshToken}})
		if oauthErr, ok := errors.AsType[*oauthError](err); ok && slices.Contains(lostGrant, oauthErr.code) {
			logger.Warnf(ctx, "grant %s of %s (%s) needs a reconnect: %v", g.id, g.user, g.connector, err)
			g.status = statusReconnect
			return g, errReconnect
		}
		if err != nil {
			return nil, err
		}
		rotated = t.RefreshToken != "" && t.RefreshToken != sec.RefreshToken
		sec.AccessToken, sec.RefreshToken = t.AccessToken, cmp.Or(t.RefreshToken, sec.RefreshToken)
		if g.sealed, err = s.seal.seal(ctx, g.user, sec, grantData(g.id, g.user, g.connector)); err != nil {
			return nil, err
		}
		g.expiresAt = expiry(t.ExpiresIn)
		access = t.AccessToken
		logger.Infof(ctx, "refreshed grant %s of %s (%s), refresh token rotated: %t", g.id, g.user, g.connector, rotated)
		return g, nil
	})
	if err != nil && rotated {
		logger.Errorf(ctx, err, "store the rotated refresh token of grant %s", id)
		err = errors.Join(err, s.store.setStatus(context.WithoutCancel(ctx), id, statusReconnect))
	}
	return access, err
}

// lose marks g needs_reconnect after its server refused a fresh credential.
func (s *Service) lose(ctx context.Context, g *grant) error {
	if err := s.store.setStatus(ctx, g.id, statusReconnect); err != nil {
		return err
	}
	log.WithFunc("connect.Service.lose").Warnf(ctx, "grant %s of %s (%s) needs a reconnect: its server refused the credential", g.id, g.user, g.connector)
	s.dropConn(g.id)
	s.changed(ctx, g)
	return errReconnect
}

func (s *Service) secrets(ctx context.Context, g *grant) (*secrets, error) {
	var sec secrets
	if err := s.seal.open(ctx, g.user, g.sealed, grantData(g.id, g.user, g.connector), &sec); err != nil {
		return nil, err
	}
	return &sec, nil
}

func (s *Service) client(g *grant, sec secrets) (client, error) {
	if e := s.entry(g.kind); e != nil && e.auth == authOAuth {
		return e.client, nil
	}
	if sec.Client.ID == "" {
		return client{}, fmt.Errorf("grant %s holds no client", g.id)
	}
	return sec.Client, nil
}

func (s *Service) requestApproval(ctx context.Context, g *grant, t tool, args jsontext.Value, argsSHA256 string) (*callResult, error) {
	a := &approval{
		id: "apr_" + randomHex(idBytes), user: g.user, grantID: g.id, tool: t.Name, argsSHA256: argsSHA256,
		summary: s.summary(g, t, args), expiresAt: time.Now().Add(approvalTTL),
	}
	if err := s.store.insertApproval(ctx, a); err != nil {
		return nil, err
	}
	out := errorText("Approval required: " + a.summary + ". Ask the user, then call again with the same arguments and _meta " + approvalIDMeta + ".")
	out.Meta = map[string]any{approvalMeta: map[string]string{"approval_id": a.id, "summary": a.summary}}
	return out, nil
}

// summary renders the call from its actual arguments, never from model text about them.
func (s *Service) summary(g *grant, t tool, args jsontext.Value) string {
	var fields map[string]jsontext.Value
	_ = json.Unmarshal(args, &fields)
	parts := make([]string, 0, len(fields))
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		parts = append(parts, k+"="+shorten(string(fields[k]), summaryValue))
	}
	text := s.vendorName(g) + ": " + cmp.Or(t.Title, t.Name)
	if len(parts) > 0 {
		text += " (" + strings.Join(parts, ", ") + ")"
	}
	return shorten(text, summaryTotal)
}

func (s *Service) vendorName(g *grant) string {
	if e := s.entry(g.kind); e != nil && g.kind != kindCustom {
		return e.name
	}
	return g.connector
}

func (s *Service) reconnectText(g *grant) *callResult {
	return errorText("Reconnect " + s.vendorName(g) + " in CocoonBox: " + s.publicURL + ConsolePath + ".")
}

func (s *Service) failureText(g *grant, err error) *callResult {
	if errors.Is(err, errReconnect) {
		return s.reconnectText(g)
	}
	return errorText(s.vendorName(g) + " failed: " + err.Error())
}

// expose marks an ask tool destructive in mode B, so the client's own prompt is the gate.
func expose(g *grant, t tool, door string) (map[string]jsontext.Value, bool) {
	name := g.connector + "_" + t.Name
	policy := t.effective(g.policy[t.Name])
	if policy == policyOff || !toolNamePattern.MatchString(name) {
		return nil, false
	}
	out := maps.Clone(t.raw)
	out["name"], _ = json.Marshal(name)
	if door == DoorMCP && policy == policyAsk {
		annotations := map[string]jsontext.Value{}
		_ = json.Unmarshal(t.raw["annotations"], &annotations)
		annotations["readOnlyHint"], annotations["destructiveHint"] = jsontext.Value("false"), jsontext.Value("true")
		out["annotations"], _ = json.Marshal(annotations)
	}
	return out, true
}

func parseTool(raw jsontext.Value) (tool, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return tool{}, err
	}
	var t struct {
		Name        string `json:"name"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Annotations struct {
			Title           string `json:"title"`
			ReadOnlyHint    bool   `json:"readOnlyHint"`
			DestructiveHint *bool  `json:"destructiveHint"`
		} `json:"annotations"`
	}
	if err := json.Unmarshal(raw, &t); err != nil || t.Name == "" {
		return tool{}, errors.New("a tool needs a name")
	}
	return tool{
		Name: t.Name, Title: cmp.Or(t.Title, t.Annotations.Title), Description: t.Description, ReadOnly: t.Annotations.ReadOnlyHint,
		Destructive: !t.Annotations.ReadOnlyHint && (t.Annotations.DestructiveHint == nil || *t.Annotations.DestructiveHint), raw: fields,
	}, nil
}

// uncertain reports whether a failed write may still have reached the vendor: a timeout, a 5xx, or a broken answer.
func uncertain(err error, ex exchange) bool {
	if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr.Op == "dial" {
		return false
	}
	_, answered := errors.AsType[*rpcError](err)
	return ex.status == 0 || ex.status/100 == 5 || (ex.status/100 == 2 && !answered)
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func errorText(text string) *callResult {
	return &callResult{Content: []textContent{{Type: "text", Text: text}}, IsError: true}
}

func expiry(seconds int64) *time.Time {
	if seconds <= 0 {
		return nil
	}
	return new(time.Now().Add(time.Duration(seconds) * time.Second))
}
