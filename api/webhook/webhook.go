// Package webhook resolves the bearer tokens of another user system through its token review endpoint, makes each user it
// names a cocoonbox user the first time, and reviews again every credential a call still holds.
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
	// maxUnknownTTL caps how long the api keeps a review of a token it does not know, so a new key works within seconds
	// and random bearers do not pile up.
	maxUnknownTTL = 5 * time.Second
	// retry is how soon a review that could not be asked again is retried; the calls keep running meanwhile.
	retry = 5 * time.Second
	// minAgain keeps a review with no ttl from being asked again on every tick.
	minAgain = time.Second
	// sweepAt is the cache size past which a new answer first drops the expired ones.
	sweepAt = 4096
)

// Accounts makes the user a review names a cocoonbox user and ends the calls of a credential the endpoint stopped admitting.
type Accounts interface {
	Ensure(ctx context.Context, idp, id string) (users.User, error)
	EndCredential(ctx context.Context, credential string)
}

// Options names the endpoint, the api's own bearer for it, and how often held credentials are checked for a review that is due.
type Options struct {
	// URL takes POST {"token", "entry"} or {"credential", "entry"} and answers 200 with a review; any other answer is an outage.
	URL string
	// Bearer is the api's own token for the endpoint; nil sends none.
	Bearer  Bearer
	Timeout time.Duration
	Every   time.Duration
}

// Hook is an auth.Resolver of the users the endpoint admits.
type Hook struct {
	opts     Options
	accounts Accounts
	http     *http.Client

	mu      sync.Mutex
	answers map[string]answer
	held    map[string]*hold
}

// New asks the endpoint at opts.URL, which must be https, and makes its users with accounts.
func New(opts Options, accounts Accounts) (*Hook, error) {
	if u, err := url.Parse(opts.URL); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("identity webhook url %q must be https://host[:port][/path]", opts.URL)
	}
	// a redirect is a non-200 like any other, so a token never follows one off https
	client := &http.Client{Timeout: opts.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Hook{opts: opts, accounts: accounts, http: client, answers: map[string]answer{}, held: map[string]*hold{}}, nil
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
	credential := string(entry) + ":" + rv.Credential
	h.remember(credential, rv.Credential, entry, rv.ttl())
	return auth.Identity{User: u.ID, Credential: credential}, nil
}

// Watch keeps reviewing credential while calls hold it; a credential the hook did not name is not its business.
func (h *Hook) Watch(credential string) func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	hd, ok := h.held[credential]
	if !ok {
		return func() {}
	}
	hd.refs++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		hd.refs--
	}
}

// Run reviews again every held credential that is due, until ctx ends, and ends the calls of one the endpoint refuses.
func (h *Hook) Run(ctx context.Context) {
	tick := time.NewTicker(h.opts.Every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for credential, hd := range h.due(time.Now()) {
			h.again(ctx, credential, hd)
		}
	}
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
	if rv.Authenticated && rv.Allowed && (rv.User == "" || rv.Credential == "") {
		return review{}, errors.New("the identity webhook admitted a token without its user or credential")
	}
	h.keep(key, answer{review: rv, until: time.Now().Add(rv.ttl())})
	return rv, nil
}

// again reviews hd's credential; a refusal ends its calls, an outage tries again soon.
func (h *Hook) again(ctx context.Context, credential string, hd hold) {
	rv, err := h.ask(ctx, map[string]string{"credential": hd.credential, "entry": string(hd.entry)})
	next := time.Now().Add(retry)
	switch {
	case err != nil:
		log.WithFunc("webhook.Hook.again").Warnf(ctx, "review %s again: %v", hd.entry, err)
	case rv.Authenticated && rv.Allowed:
		next = time.Now().Add(max(rv.ttl(), minAgain))
	default:
		h.forget(credential)
		h.accounts.EndCredential(ctx, credential)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.held[credential]; ok {
		cur.due = next
	}
}

func (h *Hook) ask(ctx context.Context, body map[string]string) (review, error) {
	req, bearer, err := h.request(ctx, body)
	if err != nil {
		return review{}, err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return review{}, fmt.Errorf("ask the identity webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized && h.opts.Bearer != nil {
		h.opts.Bearer.Rejected(bearer)
	}
	if resp.StatusCode != http.StatusOK {
		return review{}, fmt.Errorf("ask the identity webhook: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return decodeReview(resp.Body)
}

// request builds the POST of body with the api's bearer, which it also returns.
func (h *Hook) request(ctx context.Context, body map[string]string) (*http.Request, string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("encode the identity webhook request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.opts.URL, bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("build the identity webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.opts.Bearer == nil {
		return req, "", nil
	}
	bearer, err := h.opts.Bearer.Token(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("the identity webhook bearer: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	return req, bearer, nil
}

// remember names credential as the hook's, at entry, due for review after ttl.
func (h *Hook) remember(credential, opaque string, entry auth.Entry, ttl time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.held[credential]; !ok {
		h.held[credential] = &hold{credential: opaque, entry: entry, due: time.Now().Add(max(ttl, minAgain))}
	}
}

// due returns the held credentials due for review at now, and drops those no call holds.
func (h *Hook) due(now time.Time) map[string]hold {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]hold{}
	for credential, hd := range h.held {
		switch {
		case !now.After(hd.due):
		case hd.refs == 0:
			delete(h.held, credential)
		default:
			out[credential] = *hd
		}
	}
	return out
}

// forget drops held credential and every answer that named it, so its token is asked again.
func (h *Hook) forget(credential string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hd := h.held[credential]
	delete(h.held, credential)
	for key, a := range h.answers {
		if a.Credential == hd.credential {
			delete(h.answers, key)
		}
	}
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

// review is the endpoint's answer for a token or a credential at one entry.
type review struct {
	Authenticated bool   `json:"authenticated"`
	Allowed       bool   `json:"allowed"`
	IDP           string `json:"idp,omitempty"`
	User          string `json:"user,omitempty"`
	Credential    string `json:"credential,omitempty"`
	Kind          string `json:"kind,omitempty"`
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

// hold is a credential calls hold at one entry, reviewed again when due.
type hold struct {
	credential string
	entry      auth.Entry
	refs       int
	due        time.Time
}
