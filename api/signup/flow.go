package signup

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"
)

const (
	reasonClosed     = "signup_closed"
	reasonUnverified = "email_unverified"
	reasonNotAllowed = "email_not_allowed"
	reasonInvite     = "invite_required"
	reasonRejected   = "declined"
	reasonDisabled   = "disabled"
	reasonTaken      = "github_taken"
	reasonLinked     = "github_linked"
	defaultReturn    = "/cb/"
	maxReturn        = 2048
	maxNameLen       = 32
	maxNameTries     = 50
)

// BeginRequest starts a sign-in: Link names the signed-in user who connects GitHub, empty for a sign-in; ReturnTo is a
// console path to go back to, and Invite the code of an invite link.
type BeginRequest struct {
	Link     string
	ReturnTo string
	Invite   string
}

// Started is where the browser goes and the nonce its cookie carries back to the callback.
type Started struct {
	URL   string
	Nonce string
}

// Identity is an IdP account linked to a user.
type Identity struct {
	IDP       string    `json:"idp"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

type flow struct {
	state        string
	verifier     string
	linkUser     string
	returnTo     string
	inviteSHA256 string
}

// Begin stores a sign-in that lasts flowTTL and returns the GitHub authorize URL.
func (s *Service) Begin(ctx context.Context, req BeginRequest) (Started, error) {
	if !s.github.configured() {
		return Started{}, ErrOff
	}
	f := flow{state: randomHex(secretBytes), verifier: randomHex(secretBytes), linkUser: req.Link, returnTo: safeReturn(req.ReturnTo)}
	if req.Invite != "" {
		f.inviteSHA256 = hashHex(req.Invite)
	}
	nonce := randomHex(secretBytes)
	if err := s.store.addFlow(ctx, hashHex(nonce), f, time.Now().Add(flowTTL)); err != nil {
		return Started{}, err
	}
	return Started{URL: s.github.authorize(f.state, challenge(f.verifier), s.callback), Nonce: nonce}, nil
}

// Complete finishes the sign-in that nonce began: it links the GitHub account, signs its user in, or runs signup for a new one.
func (s *Service) Complete(ctx context.Context, nonce, state, code string) (Outcome, error) {
	f, err := s.store.takeFlow(ctx, hashHex(nonce), state)
	if err != nil {
		return Outcome{}, err
	}
	id, err := s.github.identity(ctx, s.client, code, f.verifier, s.callback)
	if err != nil {
		return Outcome{}, err
	}
	if f.linkUser != "" {
		return s.link(ctx, f.linkUser, id)
	}
	user, err := s.store.identityUser(ctx, GitHub, id.subject)
	switch {
	case err == nil:
		if u, getErr := s.accounts.Get(user); getErr != nil || u.Disabled {
			return Outcome{Kind: Refused, Reason: reasonDisabled}, nil
		}
		return Outcome{Kind: SignedIn, User: user, ReturnTo: f.returnTo}, nil
	case !errors.Is(err, ErrNotFound):
		return Outcome{}, err
	}
	return s.signup(ctx, f, id)
}

// Identities returns the IdP accounts linked to user.
func (s *Service) Identities(ctx context.Context, user string) ([]Identity, error) {
	return s.store.identities(ctx, user)
}

// Unlink removes user's identity of idp.
func (s *Service) Unlink(ctx context.Context, user, idp string) error {
	return s.store.removeIdentity(ctx, user, idp)
}

func (s *Service) link(ctx context.Context, user string, id identity) (Outcome, error) {
	taken, err := s.store.addIdentity(ctx, s.store.pool, user, GitHub, id)
	switch {
	case err != nil:
		return Outcome{}, err
	case taken == takenSubject:
		return Outcome{Kind: Refused, Reason: reasonTaken}, nil
	case taken == takenUser:
		return Outcome{Kind: Refused, Reason: reasonLinked}, nil
	}
	if err := s.store.dropWaiting(ctx, GitHub, id.subject); err != nil {
		return Outcome{}, err
	}
	return Outcome{Kind: Linked, User: user}, nil
}

// signup queues a new identity, at the front with an invite, and admits; the person is signed in when a seat took them. A person
// already waiting gets a new secret for their place.
func (s *Service) signup(ctx context.Context, f flow, id identity) (Outcome, error) {
	secret := queuePrefix + randomHex(secretBytes)
	status, err := s.store.requeue(ctx, GitHub, id.subject, hashHex(secret))
	switch {
	case err != nil:
		return Outcome{}, err
	case status == statusRejected:
		return Outcome{Kind: Refused, Reason: reasonRejected}, nil
	case status == statusWaiting:
		return Outcome{Kind: Queued, QueueSecret: secret}, nil
	case status == statusAdmitted:
		return Outcome{Kind: Refused, Reason: reasonClosed}, nil
	}
	st, err := s.Settings(ctx)
	switch {
	case err != nil:
		return Outcome{}, err
	case !st.Enabled:
		return Outcome{Kind: Refused, Reason: reasonClosed}, nil
	case id.email == "" || !id.verified:
		return Outcome{Kind: Refused, Reason: reasonUnverified}, nil
	case !st.allows(id.email):
		return Outcome{Kind: Refused, Reason: reasonNotAllowed}, nil
	}
	queued, err := s.store.enqueue(ctx, entry{idp: GitHub, subject: id.subject, email: id.email, nameHint: id.login, secretSHA256: hashHex(secret)}, f.inviteSHA256, st.InviteOnly)
	switch {
	case errors.Is(err, errNoInvite):
		return Outcome{Kind: Refused, Reason: reasonInvite}, nil
	case err != nil:
		return Outcome{}, err
	}
	if err = s.Admit(ctx); err != nil {
		return Outcome{}, err
	}
	user, err := s.store.admittedUser(ctx, queued)
	if err != nil {
		return Outcome{}, err
	}
	if user == "" {
		return Outcome{Kind: Queued, QueueSecret: secret}, nil
	}
	return Outcome{Kind: SignedIn, User: user, ReturnTo: f.returnTo}, nil
}

// safeReturn keeps a console path and nothing else, so the callback never sends the browser off the origin.
func safeReturn(path string) string {
	if len(path) > maxReturn || !strings.HasPrefix(path, defaultReturn) || strings.ContainsAny(path, "\\\r\n") || strings.HasPrefix(path, "//") {
		return defaultReturn
	}
	return path
}

// usernames yields the GitHub login cleaned to a username, then with -2, -3, … for a clash.
func usernames(login string) iter.Seq[string] {
	var b strings.Builder
	for _, r := range strings.ToLower(login) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	cleaned := b.String()
	base := strings.TrimLeft(cleaned, "-_0123456789")
	if len(base) < 2 {
		base = "gh-" + strings.Trim(cleaned, "-_")
	}
	base = strings.TrimRight(base[:min(len(base), maxNameLen-3)], "-")
	return func(yield func(string) bool) {
		if !yield(base) {
			return
		}
		for i := 2; i <= maxNameTries; i++ {
			if !yield(fmt.Sprintf("%s-%d", base, i)) {
				return
			}
		}
	}
}
