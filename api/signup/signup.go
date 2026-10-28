// Package signup signs people in with GitHub and, while an admin has it on, lets a new person sign up: a seat makes the
// account at once, and with every seat taken the person waits in an ordered queue that admission drains as seats open.
package signup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/users"
)

const (
	// GitHub is the one identity provider.
	GitHub = "github"
	// CallbackPath takes the IdP's answer outside /cb/, so the Lax nonce cookie reaches it.
	CallbackPath = "/cocoonbox.auth/callback"
	// CookiePath scopes the nonce cookie of a sign-in to the callback.
	CookiePath = "/cocoonbox.auth/"

	// SignedIn is a known identity, or one admission just made an account for.
	SignedIn Kind = "signed_in"
	// Linked is an identity the signed-in user connected to the account.
	Linked Kind = "linked"
	// Queued is a person waiting for a seat.
	Queued Kind = "queued"
	// Refused carries the reason in Outcome.Reason.
	Refused Kind = "refused"

	migrationLock = 0x63627367 // "cbsg"
	secretBytes   = 32
	idBytes       = 8
	flowTTL       = 10 * time.Minute
	fetchTimeout  = 10 * time.Second
	admitEvery    = 5 * time.Minute
	maxListed     = 200
	maxSeats      = 10000
	maxAgentBoxes = 16
	maxInviteUses = 1000
	maxInviteDays = 90
	maxAllowed    = 100
	invitePrefix  = "cbi_"
	queuePrefix   = "cbq_"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid request")
	// ErrOff is a sign-in with GitHub while no login client is configured.
	ErrOff = errors.New("sign-in with GitHub is not configured")

	errExpired = errors.New("the sign-in expired or did not start in this browser")
)

// Kind is how a sign-in through GitHub ended.
type Kind string

// Accounts creates and reads users; admission makes each admitted person's account with it.
type Accounts interface {
	Create(ctx context.Context, nu users.NewUser) (users.User, error)
	Get(id string) (users.User, error)
}

// Options names the database, the public origin of the callback, the login client and the users.
type Options struct {
	DSN       string
	PublicURL string
	GitHub    GitHubClient
	Accounts  Accounts
}

// Settings are the admin's signup switches and the defaults of a signup account.
type Settings struct {
	Enabled        bool     `json:"enabled"`
	InviteOnly     bool     `json:"invite_only"`
	Seats          int      `json:"seats"`
	AllowDomains   []string `json:"allow_domains"`
	AllowEmails    []string `json:"allow_emails"`
	DailyCapMicros int64    `json:"daily_cap_micros"`
	MaxAgentBoxes  int      `json:"max_agent_boxes"`
	HourlyAccounts int      `json:"hourly_accounts"`
}

func (st Settings) check() error {
	switch {
	case st.Seats < 0 || st.Seats > maxSeats:
		return fmt.Errorf("%w: seats must be in 0..%d", ErrInvalid, maxSeats)
	case st.DailyCapMicros < 0:
		return fmt.Errorf("%w: the daily cap is at least 0", ErrInvalid)
	case st.MaxAgentBoxes < 1 || st.MaxAgentBoxes > maxAgentBoxes:
		return fmt.Errorf("%w: max agent boxes must be in 1..%d", ErrInvalid, maxAgentBoxes)
	case st.HourlyAccounts < 1 || st.HourlyAccounts > maxSeats:
		return fmt.Errorf("%w: accounts per hour must be in 1..%d", ErrInvalid, maxSeats)
	case len(st.AllowDomains) > maxAllowed || len(st.AllowEmails) > maxAllowed:
		return fmt.Errorf("%w: at most %d allowed domains and %d allowed emails", ErrInvalid, maxAllowed, maxAllowed)
	}
	return nil
}

// allows reports whether email may sign up: empty lists allow everyone.
func (st Settings) allows(email string) bool {
	if len(st.AllowDomains) == 0 && len(st.AllowEmails) == 0 {
		return true
	}
	_, domain, _ := strings.Cut(email, "@")
	return slices.Contains(st.AllowEmails, email) || slices.Contains(st.AllowDomains, domain)
}

// Seats counts the seats in use (enabled non-admin accounts), the cap and the people waiting.
type Seats struct {
	Used    int `json:"used"`
	Cap     int `json:"cap"`
	Waiting int `json:"waiting"`
}

// Outcome is how a GitHub sign-in ended: the user who signed in or linked, the return path, a queued person's secret, or the refusal code.
type Outcome struct {
	Kind        Kind
	User        string
	ReturnTo    string
	QueueSecret string
	Reason      string
}

// Service is sign-in with GitHub, the signup settings, the queue and the invites.
type Service struct {
	store    *store
	accounts Accounts
	github   GitHubClient
	callback string
	client   *http.Client
}

// Open connects to the database and applies the migrations.
func Open(ctx context.Context, opts Options) (*Service, error) {
	st, err := openStore(ctx, opts.DSN)
	if err != nil {
		return nil, err
	}
	return &Service{
		store: st, accounts: opts.Accounts, github: opts.GitHub, callback: strings.TrimSuffix(opts.PublicURL, "/") + CallbackPath,
		client: &http.Client{Timeout: fetchTimeout},
	}, nil
}

// Close releases the database pool.
func (s *Service) Close() { s.store.close() }

// Configured reports whether a GitHub login client is set.
func (s *Service) Configured() bool { return s.github.configured() }

// Settings returns the signup settings.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	return s.store.settings(ctx, s.store.pool)
}

// SetSettings replaces the signup settings and admits whoever the new ones make room for.
func (s *Service) SetSettings(ctx context.Context, st Settings) (Settings, error) {
	st.AllowDomains, st.AllowEmails = lowered(st.AllowDomains), lowered(st.AllowEmails)
	if err := st.check(); err != nil {
		return Settings{}, err
	}
	if err := s.store.setSettings(ctx, st); err != nil {
		return Settings{}, err
	}
	return st, s.Admit(ctx)
}

// Seats returns the seats in use, the cap and the queue's length.
func (s *Service) Seats(ctx context.Context) (Seats, error) {
	return s.store.seats(ctx)
}

// Run admits on a timer until ctx ends, so a queue the hourly brake held back moves on its own.
func (s *Service) Run(ctx context.Context) {
	logger := log.WithFunc("signup.Service.Run")
	for {
		if err := s.Admit(ctx); err != nil && ctx.Err() == nil {
			logger.Warnf(ctx, "admit: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(admitEvery):
		}
	}
}

func lowered(list []string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
