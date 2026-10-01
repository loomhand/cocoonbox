// Package gwkeys issues and revokes per-machine gw keys through gw's admin API.
package gwkeys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	tenant          = "cocoonbox-boxes"
	product         = "cocoonbox"
	qps             = 4
	dailyTokens     = 5_000_000
	tokensPerMinute = 200_000

	pageSize       = 10_000
	requestTimeout = 30 * time.Second
	maxErrorBody   = 4 << 10
)

// Client manages keys in tenant cocoonbox-boxes, one per machine, found by owner = machine id.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New talks to gw's admin API at base with a tenant admin token.
func New(base, token string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("gw admin url %q must be http(s)://host[:port]", base)
	}
	return &Client{base: u, token: token, http: &http.Client{Timeout: requestTimeout}}, nil
}

// Add replaces machine's gw keys with one that gw generates and returns it.
func (c *Client) Add(ctx context.Context, machine string) (string, error) {
	if err := c.Revoke(ctx, machine); err != nil {
		return "", err
	}
	body := map[string]any{
		"product": product, "tenant": tenant, "owner": machine,
		"qps": qps, "daily_token_quota": dailyTokens, "tokens_per_minute": tokensPerMinute,
	}
	var created struct {
		AK string `json:"ak"`
	}
	if _, err := c.request(ctx, http.MethodPost, "", nil, body, &created); err != nil {
		return "", err
	}
	if created.AK == "" {
		return "", errors.New("POST /admin/keys: no generated key in the response")
	}
	return created.AK, nil
}

// Revoke removes machine's gw keys; revoking an absent key succeeds.
func (c *Client) Revoke(ctx context.Context, machine string) error {
	ids, err := c.keysOf(ctx, machine)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.delete(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) keysOf(ctx context.Context, machine string) ([]string, error) {
	var page struct {
		Keys []struct {
			AKID   string `json:"ak_id"`
			Tenant string `json:"tenant"`
			Owner  string `json:"owner"`
		} `json:"keys"`
	}
	query := url.Values{"tenant": {tenant}, "owner": {machine}, "limit": {strconv.Itoa(pageSize)}}
	if _, err := c.request(ctx, http.MethodGet, "", query, nil, &page); err != nil {
		return nil, err
	}
	var ids []string
	for _, k := range page.Keys {
		if k.Tenant == tenant && k.Owner == machine && k.AKID != "" {
			ids = append(ids, k.AKID)
		}
	}
	return ids, nil
}

func (c *Client) delete(ctx context.Context, id string) error {
	status, err := c.request(ctx, http.MethodDelete, id, nil, nil, nil)
	if status == http.StatusNotFound {
		return nil
	}
	return err
}

// request calls /admin/keys, or /admin/keys/{id} for DELETE.
func (c *Client) request(ctx context.Context, method, id string, query url.Values, body, out any) (int, error) {
	op := method + " /admin/keys"
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode %s: %w", op, err)
		}
		payload = bytes.NewReader(raw)
	}
	u := c.base.JoinPath("admin", "keys")
	if method == http.MethodDelete {
		u = u.JoinPath(id)
	}
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), payload)
	if err != nil {
		return 0, fmt.Errorf("build %s: %w", op, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		if json.Unmarshal(raw, &e) != nil || e.Error.Message == "" {
			e.Error.Message = http.StatusText(resp.StatusCode)
		}
		return resp.StatusCode, fmt.Errorf("%s: %d %s", op, resp.StatusCode, e.Error.Message)
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode %s: %w", op, err)
	}
	return resp.StatusCode, nil
}
