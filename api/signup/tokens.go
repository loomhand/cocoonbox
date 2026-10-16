package signup

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// InviteTokenPrefix starts every invite-scoped token, which no user token check accepts.
	InviteTokenPrefix = "cbit_"

	tokenInviteDays    = 2
	maxTokenInviteDays = 7
	maxTokenLabel      = 100
	tokenUsedEvery     = time.Minute
)

// InviteToken is an admin's invite-scoped token as the admin page lists it; the token is shown only when it is minted.
type InviteToken struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	UsedAt    *time.Time `json:"used_at"`
	Invites   int        `json:"invites"`
}

// MintInviteToken makes an invite-scoped token for admin, labeled label, and returns it once.
func (s *Service) MintInviteToken(ctx context.Context, admin, label string) (string, InviteToken, error) {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > maxTokenLabel || !utf8.ValidString(label) {
		return "", InviteToken{}, fmt.Errorf("%w: a label is 1 to %d bytes", ErrInvalid, maxTokenLabel)
	}
	token := InviteTokenPrefix + randomHex(secretBytes)
	t := InviteToken{ID: "itk_" + randomHex(idBytes), Label: label, CreatedBy: admin}
	minted, err := s.store.addInviteToken(ctx, t, hashHex(token))
	return token, minted, err
}

// InviteTokens lists the invite-scoped tokens, newest first.
func (s *Service) InviteTokens(ctx context.Context) ([]InviteToken, error) {
	return s.store.inviteTokens(ctx)
}

// RevokeInviteToken deletes token id; the invites it made stay usable.
func (s *Service) RevokeInviteToken(ctx context.Context, id string) error {
	return s.store.removeInviteToken(ctx, id)
}

// Inviter returns the id of the invite-scoped token, ErrNotFound unless it is live and its admin is still an enabled admin.
func (s *Service) Inviter(ctx context.Context, token string) (string, error) {
	if !strings.HasPrefix(token, InviteTokenPrefix) {
		return "", ErrNotFound
	}
	id, admin, used, err := s.store.inviteTokenBySHA256(ctx, hashHex(token))
	if err != nil {
		return "", err
	}
	if u, err := s.accounts.Get(admin); err != nil || !u.Admin || u.Disabled {
		return "", ErrNotFound
	}
	if used == nil || time.Since(*used) > tokenUsedEvery {
		if err := s.store.touchInviteToken(ctx, id); err != nil {
			return "", err
		}
	}
	return id, nil
}

// CreateTokenInvite makes a one-use invite for token id that lasts days, 0 for the default 2, at most 7, and returns its code once.
func (s *Service) CreateTokenInvite(ctx context.Context, id string, days int) (string, Invite, error) {
	days = cmp.Or(days, tokenInviteDays)
	if days < 1 || days > maxTokenInviteDays {
		return "", Invite{}, fmt.Errorf("%w: an invite made with a token lasts 1 to %d days", ErrInvalid, maxTokenInviteDays)
	}
	return s.createInvite(ctx, Invite{UsesLeft: 1, CreatedBy: "token " + id, TokenID: id}, days)
}

// TokenInvites lists the invites token id made, newest first.
func (s *Service) TokenInvites(ctx context.Context, id string) ([]Invite, error) {
	return s.store.invites(ctx, id)
}
