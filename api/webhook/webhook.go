// Package webhook resolves the bearer tokens of another user system through its token review endpoint and makes each user it names a cocoonbox user.
package webhook

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/users"
	"github.com/loomhand/cocoonbox/api/utils"
)

const (
	// maxTTL caps how long the api keeps a review of a token it knows, so a revoked one opens no call a minute later.
	maxTTL = time.Minute
	// maxUnknownTTL caps how long the api keeps a review of a token it does not know, so a new key works within seconds.
	maxUnknownTTL = 5 * time.Second
	// sweepAt is the cache size past which a new answer first drops the expired ones.
	sweepAt = 4096
)

// Accounts makes the user a review names a cocoonbox user.
type Accounts interface {
	Ensure(ctx context.Context, idp, id string) (users.User, error)
}

// Options names the endpoint and the file that holds the api's own bearer for it.
type Options struct {
	// URL takes POST {"token", "entry"} and answers 200 with a review; any other answer is an outage.
	URL string
	// TokenFile holds the api's bearer, read on every call so whatever rotates it is picked up; empty sends none.
	TokenFile string
	Timeout   time.Duration
}

// Hook is an auth.Resolver of the users the endpoint admits.
type Hook struct {
	opts     Options
	accounts Accounts
	http     *http.Client

	mu      sync.Mutex
	answers map[string]answer
}

// New asks the endpoint at opts.URL, which must be https, and makes its users with accounts.
func New(opts Options, accounts Accounts) (*Hook, error) {
	if u, err := url.Parse(opts.URL); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("identity webhook url %q must be https://host[:port][/path]", opts.URL)
	}
	// a redirect is a non-200 like any other, so a token never follows one off https
	client := &http.Client{Timeout: opts.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Hook{opts: opts, accounts: accounts, http: client, answers: map[string]answer{}}, nil
}

// Resolve returns the enabled user the endpoint admits token as at entry, bound to entry and the review's credential.
func (h *Hook) Resolve(ctx context.Context, token string, entry auth.Entry) (auth.Identity, error) {
	rv, err := h.tokenReview(ctx, token, entry)
	switch {
	case err != nil:
		return auth.Identity{}, err
	case !rv.Authenticated:
		return auth.Identity{}, auth.ErrUnknownToken
	case !rv.Allowed:
		return auth.Identity{}, auth.ErrNotAllowed
	}
	u, err := h.accounts.Ensure(ctx, rv.IDP, rv.User)
	switch {
	case errors.Is(err, users.ErrExists), errors.Is(err, users.ErrInvalid):
		log.WithFunc("webhook.Hook.Resolve").Warnf(ctx, "refused user %q of %q: %v", rv.User, rv.IDP, err)
		return auth.Identity{}, auth.ErrUnknownToken
	case err != nil:
		return auth.Identity{}, fmt.Errorf("ensure user %s of %s: %w", rv.User, rv.IDP, err)
	case u.Disabled:
		return auth.Identity{}, auth.ErrUnknownToken
	}
	return auth.Identity{User: u.ID, Credential: string(entry) + ":" + rv.Credential}, nil
}

// tokenReview returns the review of token at entry, from an answer still held or a new ask.
func (h *Hook) tokenReview(ctx context.Context, token string, entry auth.Entry) (review, error) {
	key := utils.HashHex(token) + " " + string(entry)
	h.mu.Lock()
	a, ok := h.answers[key]
	h.mu.Unlock()
	if ok && time.Now().Before(a.until) {
		return a.review, nil
	}
	rv, err := h.ask(ctx, map[string]string{"token": token, "entry": string(entry)})
	if err != nil {
		return review{}, err
	}
	if rv.Authenticated && rv.Allowed && (rv.IDP == "" || rv.User == "" || rv.Credential == "") {
		return review{}, errors.New("the identity webhook admitted a token without its idp, user or credential")
	}
	h.keep(key, answer{review: rv, until: time.Now().Add(rv.ttl())})
	return rv, nil
}

func (h *Hook) ask(ctx context.Context, body map[string]string) (review, error) {
	req, err := h.request(ctx, body)
	if err != nil {
		return review{}, err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return review{}, fmt.Errorf("ask the identity webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return review{}, fmt.Errorf("ask the identity webhook: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return decodeReview(resp.Body)
}

// request builds the POST of body with the api's bearer from the token file.
func (h *Hook) request(ctx context.Context, body map[string]string) (*http.Request, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode the identity webhook request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.opts.URL, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build the identity webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.opts.TokenFile == "" {
		return req, nil
	}
	file, err := os.ReadFile(h.opts.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("read the identity webhook token: %w", err)
	}
	token := strings.TrimSpace(string(file))
	if token == "" {
		return nil, fmt.Errorf("identity webhook token file %s is empty", h.opts.TokenFile)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

func (h *Hook) keep(key string, a answer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.answers) >= sweepAt {
		now := time.Now()
		for k, old := range h.answers {
			if !now.Before(old.until) {
				delete(h.answers, k)
			}
		}
	}
	h.answers[key] = a
}

// review is the endpoint's answer for a token at one entry.
type review struct {
	Authenticated bool   `json:"authenticated"`
	Allowed       bool   `json:"allowed"`
	IDP           string `json:"idp,omitempty"`
	User          string `json:"user,omitempty"`
	Credential    string `json:"credential,omitempty"`
	TTLSeconds    int    `json:"ttlSeconds"`
}

// ttl is how long rv is kept: its ttlSeconds, capped at a minute for a token the endpoint knows and at seconds for one it does not.
func (rv review) ttl() time.Duration {
	limit := maxUnknownTTL
	if rv.Authenticated {
		limit = maxTTL
	}
	return min(max(time.Duration(rv.TTLSeconds)*time.Second, 0), limit)
}

// answer is a review the api keeps until until.
type answer struct {
	review
	until time.Time
}

// decodeReview reads a review; only an explicit false refuses, and one missing its verdict is an outage, which never ends a call.
func decodeReview(body io.Reader) (review, error) {
	var verdict struct {
		review
		Authenticated *bool `json:"authenticated"`
		Allowed       *bool `json:"allowed"`
	}
	if err := json.UnmarshalRead(body, &verdict); err != nil {
		return review{}, fmt.Errorf("decode the identity webhook review: %w", err)
	}
	if verdict.Authenticated == nil || *verdict.Authenticated && verdict.Allowed == nil {
		return review{}, errors.New("the identity webhook review lacks authenticated or allowed")
	}
	rv := verdict.review
	rv.Authenticated, rv.Allowed = *verdict.Authenticated, verdict.Allowed != nil && *verdict.Allowed
	return rv, nil
}
