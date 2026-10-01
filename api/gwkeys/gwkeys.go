// Package gwkeys issues and revokes per-machine gw keys through gw's admin API.
package gwkeys

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	tenant          = "cocoonbox-boxes"
	product         = "cocoonbox"
	qps             = 4
	dailyTokens     = 5_000_000
	tokensPerMinute = 200_000

	keyPrefix = "cb-"
	keyChars  = 45
	alphabet  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

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

// Add returns machine's gw key, creating it on first use and revoking any extra.
func (c *Client) Add(ctx context.Context, machine string) (string, error) {
	keys, err := c.keysOf(ctx, machine)
	if err != nil {
		return "", err
	}
	if len(keys) > 0 {
		for _, extra := range keys[1:] {
			if err := c.delete(ctx, extra); err != nil {
				return "", err
			}
		}
		return keys[0], nil
	}
	key := newKey()
	body := map[string]any{
		"ak": key, "product": product, "tenant": tenant, "owner": machine,
		"qps": qps, "daily_token_quota": dailyTokens, "tokens_per_minute": tokensPerMinute,
	}
	if _, err := c.request(ctx, http.MethodPost, key, nil, body, nil); err != nil {
		return "", err
	}
	return key, nil
}

// Revoke removes machine's gw keys; revoking an absent key succeeds.
func (c *Client) Revoke(ctx context.Context, machine string) error {
	keys, err := c.keysOf(ctx, machine)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := c.delete(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) keysOf(ctx context.Context, machine string) ([]string, error) {
	var keys []string
	for offset := 0; ; offset += pageSize {
		var page struct {
			Keys []struct {
				AK     string `json:"ak"`
				Tenant string `json:"tenant"`
				Owner  string `json:"owner"`
			} `json:"keys"`
		}
		query := url.Values{"tenant": {tenant}, "offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(pageSize)}}
		if _, err := c.request(ctx, http.MethodGet, "", query, nil, &page); err != nil {
			return nil, err
		}
		for _, k := range page.Keys {
			if k.Tenant == tenant && k.Owner == machine && k.AK != "" {
				keys = append(keys, k.AK)
			}
		}
		if len(page.Keys) < pageSize {
			return keys, nil
		}
	}
}

func (c *Client) delete(ctx context.Context, key string) error {
	status, err := c.request(ctx, http.MethodDelete, key, nil, nil, nil)
	if status == http.StatusNotFound {
		return nil
	}
	return err
}

// request calls /admin/keys, or /admin/keys/{key} for DELETE; its errors never carry key.
func (c *Client) request(ctx context.Context, method, key string, query url.Values, body, out any) (int, error) {
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
		u = u.JoinPath(key)
	}
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), payload)
	if err != nil {
		return 0, fmt.Errorf("build %s: %w", op, redact(err, key))
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
		return 0, fmt.Errorf("%s: %w", op, redact(err, key))
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
		return resp.StatusCode, fmt.Errorf("%s: %d %s", op, resp.StatusCode, redact(errors.New(e.Error.Message), key))
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode %s: %w", op, err)
	}
	return resp.StatusCode, nil
}

func redact(err error, key string) error {
	if key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "<key>"))
}

// newKey draws keyChars characters from alphabet without modulo bias.
func newKey() string {
	key := make([]byte, 0, len(keyPrefix)+keyChars)
	key = append(key, keyPrefix...)
	buf := make([]byte, keyChars)
	for len(key) < cap(key) {
		_, _ = rand.Read(buf)
		for _, b := range buf {
			if int(b) < 256-256%len(alphabet) && len(key) < cap(key) {
				key = append(key, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	return string(key)
}
