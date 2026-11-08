package sandboxd

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	dialTimeout  = 5 * time.Second
	maxErrorBody = 4 << 10

	diskLowReason = "disk below reserve"
)

type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return fmt.Sprintf("sandboxd %d: %s", e.status, e.message) }

func (e *apiError) Unwrap() error {
	switch e.status {
	case http.StatusBadRequest:
		return machines.ErrRefused
	case http.StatusNotFound:
		return machines.ErrNotFound
	case http.StatusConflict:
		return machines.ErrConflict
	case http.StatusTooManyRequests:
		return machines.ErrCapacity
	default:
		return nil
	}
}

type poolKey struct {
	Template string `json:"template"`
	Net      string `json:"net"`
	Size     string `json:"size"`
}

// row is one claim of the sandboxd index; Token is set only on the root by-id read.
type row struct {
	ID         string            `json:"id"`
	Key        poolKey           `json:"key"`
	Tenant     string            `json:"tenant,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Hibernated bool              `json:"hibernated"`
	Archived   bool              `json:"archived,omitzero"`
	Restarts   int               `json:"restarts,omitzero"`
	Failed     string            `json:"failed,omitempty"`
	ClaimedAt  time.Time         `json:"claimed_at,omitzero"`
	EnvMissing bool              `json:"env_missing,omitzero"`
}

type envVar struct {
	Value   string  `json:"value"`
	Guest   *bool   `json:"guest,omitempty"`
	Inject  *inject `json:"inject,omitempty"`
	Missing bool    `json:"missing,omitzero"`
}

type inject struct {
	Hosts       []string `json:"hosts"`
	Header      string   `json:"header,omitempty"`
	Query       string   `json:"query,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
}

type claimRequest struct {
	poolKey
	TTLSeconds      int               `json:"ttl_seconds"`
	OnExpire        string            `json:"on_expire"`
	Metadata        map[string]string `json:"metadata"`
	NoRedirect      bool              `json:"no_redirect,omitzero"`
	RequirePromoted bool              `json:"require_promoted,omitzero"`
}

// claimResponse is the claim a node made, or the peers to claim on instead.
type claimResponse struct {
	ID              string   `json:"id"`
	OwnerAddr       string   `json:"owner_addr"`
	Redirect        []string `json:"redirect"`
	RequirePromoted bool     `json:"require_promoted"`
}

type renewRequest struct {
	TTLSeconds int    `json:"ttl_seconds"`
	OnExpire   string `json:"on_expire"`
}

type execRequest struct {
	Argv           []string          `json:"argv"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

type execResponse struct {
	ExitCode int32  `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type tenantRequest struct {
	Token       string `json:"token,omitempty"`
	MaxClaims   int    `json:"max_claims"`
	EgressClass string `json:"egress_class,omitempty"`
}

// client speaks the sandboxd HTTP API with the node's root token.
type client struct {
	base  *url.URL
	token string
	http  *http.Client
}

func newClient(rawURL, token string) (*client, error) {
	base, err := url.Parse(rawURL)
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("sandboxd url %q must be an absolute URL", rawURL)
	}
	return &client{base: base, token: token, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *client) claim(ctx context.Context, token string, req claimRequest) (claimResponse, error) {
	var resp claimResponse
	err := c.request(ctx, cmp.Or(token, c.token), http.MethodPost, "/v1/claim", req, &resp)
	return resp, err
}

// peers returns the other nodes of the node's mesh, none for a node without one.
func (c *client) peers(ctx context.Context) ([]string, error) {
	var resp struct {
		Peers []string `json:"peers"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/peers", nil, &resp)
	return resp.Peers, err
}

// advertised returns the address the node gives clients and its mesh names it by, "" for a node that advertises none.
func (c *client) advertised(ctx context.Context) (string, error) {
	var resp struct {
		AdvertiseAddr string `json:"advertise_addr"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/info", nil, &resp)
	return resp.AdvertiseAddr, err
}

// full names why the node has no room for another running sandbox, "" while it has room or only its disk is low, which a hibernation does not relieve.
func (c *client) full(ctx context.Context) (string, error) {
	var resp struct {
		AtCapacity       bool   `json:"at_capacity"`
		AtCapacityReason string `json:"at_capacity_reason"`
		Running          int    `json:"running"`
		MaxRunning       int    `json:"max_running"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/info", nil, &resp); err != nil {
		return "", err
	}
	switch {
	case resp.AtCapacity && !strings.HasPrefix(resp.AtCapacityReason, diskLowReason):
		return cmp.Or(resp.AtCapacityReason, "at capacity"), nil
	case resp.MaxRunning > 0 && resp.Running >= resp.MaxRunning:
		return fmt.Sprintf("%d of %d sandboxes running", resp.Running, resp.MaxRunning), nil
	}
	return "", nil
}

// owner asks the node which node owns sandbox id, with the sandbox's own token; a node may adopt an archived claim to answer.
func (c *client) owner(ctx context.Context, id string) (string, error) {
	var resp struct {
		OwnerAddr string `json:"owner_addr"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id)+"/owner", nil, &resp)
	return resp.OwnerAddr, err
}

func (c *client) sandboxes(ctx context.Context) ([]row, error) {
	var resp struct {
		Sandboxes []row `json:"sandboxes"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/sandboxes", nil, &resp)
	return resp.Sandboxes, err
}

func (c *client) get(ctx context.Context, id string) (row, error) {
	var r row
	err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), nil, &r)
	return r, err
}

func (c *client) verb(ctx context.Context, id, verb string, body any) error {
	return c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/"+verb, body, nil)
}

func (c *client) env(ctx context.Context, id string) (map[string]envVar, error) {
	var resp struct {
		Env map[string]envVar `json:"env"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id)+"/env", nil, &resp)
	return resp.Env, err
}

// patchEnv merges entries into the claim's env; a nil entry removes the name.
func (c *client) patchEnv(ctx context.Context, id string, entries map[string]*envVar) error {
	return c.do(ctx, http.MethodPatch, "/v1/sandboxes/"+url.PathEscape(id)+"/env", map[string]any{"env": entries}, nil)
}

// exec runs a buffered command as the guest's root with the sandbox's own token.
func (c *client) exec(ctx context.Context, id string, req execRequest) (execResponse, error) {
	var resp execResponse
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec", req, &resp)
	return resp, err
}

// dialPort opens the passive port relay to guest port on sandbox id.
func (c *client) dialPort(ctx context.Context, id string, port int) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", c.base.Host)
	if err != nil {
		return nil, fmt.Errorf("dial sandboxd: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	}
	path := "/v1/sandboxes/" + url.PathEscape(id) + "/ports/" + strconv.Itoa(port)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", path, c.base.Host, c.token)
	if _, err = io.WriteString(conn, req); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send relay upgrade: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read relay upgrade: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = conn.Close() }()
		return nil, responseError(resp)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	return c.request(ctx, c.token, method, path, body, out)
}

func (c *client) request(ctx context.Context, token, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(raw)
	}
	p, query, _ := strings.Cut(path, "?")
	u := c.base.JoinPath(p)
	u.RawQuery = query
	req, err := http.NewRequestWithContext(ctx, method, u.String(), payload)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %w", method, path, responseError(resp))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.UnmarshalRead(resp.Body, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

// bufferedConn keeps the bytes the upgrade reader buffered past the 101.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite half-closes the relay, which sandboxd carries to the guest port.
func (c *bufferedConn) CloseWrite() error {
	if tc, ok := c.Conn.(*net.TCPConn); ok {
		return tc.CloseWrite()
	}
	return c.Close()
}

func nodeURL(addr string) string {
	if strings.Contains(addr, "://") {
		return addr
	}
	return "http://" + addr
}

func responseError(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if json.Unmarshal(raw, &body) != nil || body.Error == "" {
		body.Error = http.StatusText(resp.StatusCode)
	}
	return &apiError{status: resp.StatusCode, message: body.Error}
}

// transient reports whether a claim that failed with err may succeed on another node, as the sandboxd SDK decides.
func transient(err error) bool {
	apiErr, ok := errors.AsType[*apiError](err)
	if !ok {
		return true
	}
	switch apiErr.status {
	case http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
