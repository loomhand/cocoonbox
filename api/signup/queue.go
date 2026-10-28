package signup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/users"
)

const (
	statusWaiting  = "waiting"
	statusAdmitted = "admitted"
	statusRejected = "rejected"

	// ActionAdmit makes the account now, past the cap and the hourly brake.
	ActionAdmit = "admit"
	// ActionFront moves the person to the front of the queue.
	ActionFront = "front"
	// ActionReject declines the person, who is refused on every later sign-in.
	ActionReject = "reject"
)

// Entry is one person in the queue as the admin page lists them.
type Entry struct {
	ID        string    `json:"id"`
	IDP       string    `json:"idp"`
	Email     string    `json:"email"`
	NameHint  string    `json:"name_hint"`
	Status    string    `json:"status"`
	Position  int       `json:"position"`
	Invited   bool      `json:"invited"`
	CreatedAt time.Time `json:"created_at"`
}

// Place is what a queued person sees of their own entry.
type Place struct {
	Status   string    `json:"status"`
	Position int       `json:"position"`
	Waiting  int       `json:"waiting"`
	Email    string    `json:"email"`
	Since    time.Time `json:"since"`
}

// Invite is an invite as the admin page lists it, with the invite-scoped token that made it; the code is shown only when it is made.
type Invite struct {
	ID        string    `json:"id"`
	UsesLeft  int       `json:"uses_left"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedBy string    `json:"created_by"`
	TokenID   string    `json:"token_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type entry struct {
	id           string
	idp          string
	subject      string
	email        string
	nameHint     string
	secretSHA256 string
}

// Admit gives the free seats, within the hourly brake, to the oldest waiting people. The settings row's lock orders
// admissions across apis, so one freed seat admits one person; it runs past the caller's cancel, and each entry in its own
// savepoint, so one failure leaves the others admitted.
func (s *Service) Admit(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	return pgx.BeginFunc(ctx, s.store.pool, func(tx pgx.Tx) error {
		st, err := s.store.lockSettings(ctx, tx)
		if err != nil || !st.Enabled {
			return err
		}
		free, err := s.store.room(ctx, tx, st)
		if err != nil || free <= 0 {
			return err
		}
		waiting, err := s.store.waiting(ctx, tx, free)
		if err != nil {
			return err
		}
		for _, e := range waiting {
			if err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error { return s.admit(ctx, sp, st, e) }); err != nil {
				log.WithFunc("signup.Service.Admit").Warnf(ctx, "admit queue entry %s: %v", e.id, err)
			}
		}
		return nil
	})
}

// Queue lists the waiting people in order, then the decided ones newest first.
func (s *Service) Queue(ctx context.Context) ([]Entry, error) {
	return s.store.queue(ctx)
}

// Act applies an admin's action to queue entry id; admit makes the account even with every seat taken.
func (s *Service) Act(ctx context.Context, id, action string) error {
	switch action {
	case ActionFront:
		return s.store.toFront(ctx, id)
	case ActionReject:
		return s.store.reject(ctx, id)
	case ActionAdmit:
		return pgx.BeginFunc(ctx, s.store.pool, func(tx pgx.Tx) error {
			st, err := s.store.lockSettings(ctx, tx)
			if err != nil {
				return err
			}
			e, err := s.store.waitingEntry(ctx, tx, id)
			if err != nil {
				return err
			}
			return s.admit(ctx, tx, st, e)
		})
	}
	return fmt.Errorf("%w: an action is %s, %s or %s", ErrInvalid, ActionAdmit, ActionFront, ActionReject)
}

// Place returns the entry of the queue secret.
func (s *Service) Place(ctx context.Context, secret string) (Place, error) {
	return s.store.place(ctx, hashHex(secret))
}

// Leave deletes the waiting entry of the queue secret, keeping nothing of it, and admits the next person in its stead.
func (s *Service) Leave(ctx context.Context, secret string) error {
	if err := s.store.leave(ctx, hashHex(secret)); err != nil {
		return err
	}
	return s.Admit(ctx)
}

// CreateInvite makes an invite of uses sign-ups that lasts days, and returns its code once.
func (s *Service) CreateInvite(ctx context.Context, by string, uses, days int) (string, Invite, error) {
	if uses < 1 || uses > maxInviteUses || days < 1 || days > maxInviteDays {
		return "", Invite{}, fmt.Errorf("%w: an invite has 1 to %d uses and lasts 1 to %d days", ErrInvalid, maxInviteUses, maxInviteDays)
	}
	return s.createInvite(ctx, Invite{UsesLeft: uses, CreatedBy: by}, days)
}

// Invites lists the invites, newest first.
func (s *Service) Invites(ctx context.Context) ([]Invite, error) {
	return s.store.invites(ctx, "")
}

// RevokeInvite deletes invite id; the people it queued keep their places.
func (s *Service) RevokeInvite(ctx context.Context, id string) error {
	return s.store.removeInvite(ctx, id)
}

func (s *Service) createInvite(ctx context.Context, inv Invite, days int) (string, Invite, error) {
	code := invitePrefix + randomHex(secretBytes)
	inv.ID, inv.ExpiresAt = "inv_"+randomHex(idBytes), time.Now().AddDate(0, 0, days).Truncate(time.Microsecond)
	created, err := s.store.addInvite(ctx, inv, hashHex(code))
	return code, created, err
}

// admit makes e's account under the first free username, links its identity and marks e admitted; the id comes from e's, so a retry reuses the account.
func (s *Service) admit(ctx context.Context, tx pgx.Tx, st Settings, e entry) error {
	linked, err := s.store.identityUser(ctx, tx, e.idp, e.subject)
	if err == nil {
		return s.store.markAdmitted(ctx, tx, e.id, linked)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	id := "u-" + strings.TrimPrefix(e.id, "sq_")
	u, err := s.accounts.Get(id)
	if errors.Is(err, users.ErrNotFound) {
		err = users.ErrExists
		for name := range usernames(e.nameHint) {
			u, err = s.accounts.Create(ctx, users.NewUser{ID: id, Name: name, IDP: e.idp, MaxAgentBoxes: new(st.MaxAgentBoxes), DailyCapMicros: new(st.DailyCapMicros)})
			if !errors.Is(err, users.ErrExists) {
				break
			}
		}
	}
	if err != nil {
		return fmt.Errorf("create the account: %w", err)
	}
	if _, err := s.store.addIdentity(ctx, tx, u.ID, e.idp, identity{subject: e.subject, email: e.email, verified: true}); err != nil {
		return fmt.Errorf("link the identity of account %s: %w", u.ID, err)
	}
	if err := s.store.markAdmitted(ctx, tx, e.id, u.ID); err != nil {
		return fmt.Errorf("mark account %s admitted: %w", u.ID, err)
	}
	log.WithFunc("signup.Service.admit").Infof(ctx, "admitted queue entry %s as %s (%s)", e.id, u.Name, u.ID)
	return nil
}
