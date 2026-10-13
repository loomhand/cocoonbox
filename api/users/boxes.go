package users

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
)

const (
	// FirstBoxName names the box every user starts with.
	FirstBoxName = "main"

	reservedBoxPrefix = "cb-"
)

var boxNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Box is one of a user's boxes: its id names its machines and its state, its name is unique among the owner's boxes.
type Box struct {
	ID    string
	Owner string
	Name  string
	// Agent marks a box an agent's grant created: the grant's caps bound it, the owner's max boxes do not.
	Agent     bool
	CreatedAt time.Time
}

// Boxes returns owner's boxes, oldest first.
func (s *Service) Boxes(owner string) []Box {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.boxes[owner])
}

// HasBox reports whether box id is one of owner's boxes.
func (s *Service) HasBox(owner, id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.ContainsFunc(s.boxes[owner], func(b Box) bool { return b.ID == id })
}

// AddBox gives b.Owner the box b, refused with ErrQuota once the owner has max boxes of its own; an agent's box raises
// the owner's tenant cap by one.
func (s *Service) AddBox(ctx context.Context, b Box) (Box, error) {
	if err := checkBoxName(b.Name); err != nil {
		return Box{}, err
	}
	if _, err := s.record(b.Owner); err != nil {
		return Box{}, err
	}
	b.CreatedAt = time.Now().Truncate(time.Microsecond)
	if err := s.store.insertBox(ctx, b); err != nil {
		return Box{}, err
	}
	s.mu.Lock()
	s.putBox(b)
	s.mu.Unlock()
	if !b.Agent {
		return b, nil
	}
	if err := s.putClaimCap(ctx, b.Owner); err != nil {
		return Box{}, errors.Join(fmt.Errorf("raise the tenant cap: %w", err), s.RemoveBox(context.WithoutCancel(ctx), b.Owner, b.ID))
	}
	return b, nil
}

// RenameBox names owner's box id name.
func (s *Service) RenameBox(ctx context.Context, owner, id, name string) (Box, error) {
	if err := checkBoxName(name); err != nil {
		return Box{}, err
	}
	if !s.HasBox(owner, id) {
		return Box{}, ErrNotFound
	}
	if err := s.store.renameBox(ctx, owner, id, name); err != nil {
		return Box{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.boxes[owner], func(b Box) bool { return b.ID == id })
	if i < 0 {
		return Box{}, ErrNotFound
	}
	b := s.boxes[owner][i]
	b.Name = name
	s.putBox(b)
	return b, nil
}

// RemoveBox removes owner's box id and revokes its share, which cuts the share's sessions; an agent's box lowers the tenant cap again.
func (s *Service) RemoveBox(ctx context.Context, owner, id string) error {
	list := s.Boxes(owner)
	i := slices.IndexFunc(list, func(b Box) bool { return b.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	agent := list[i].Agent
	if err := s.store.removeBox(ctx, owner, id); err != nil {
		return err
	}
	s.mu.Lock()
	s.boxGen++
	s.boxes[owner] = slices.DeleteFunc(slices.Clone(s.boxes[owner]), func(b Box) bool { return b.ID == id })
	var shares []string
	for _, rec := range s.shares {
		if rec.box == id {
			shares = append(shares, rec.id)
		}
	}
	s.mu.Unlock()
	for _, share := range shares {
		s.endShare(ctx, share)
	}
	if agent {
		if err := s.putClaimCap(ctx, owner); err != nil {
			log.WithFunc("users.Service.RemoveBox").Warnf(ctx, "lower the tenant cap of %s: %v", owner, err)
		}
	}
	return nil
}

// putClaimCap writes owner's tenant cap for the boxes owner has now.
func (s *Service) putClaimCap(ctx context.Context, owner string) error {
	r, err := s.record(owner)
	if err != nil {
		return err
	}
	return s.tenants.Put(ctx, owner, "", s.claimCap(r))
}

// claimCap is the tenant's max_claims: the owner's box quota, its agents' boxes, and the old machine a Rebase keeps until the new one serves.
func (s *Service) claimCap(r record) int {
	return r.MaxBoxes + s.agentBoxes(r.ID) + 1
}

func (s *Service) agentBoxes(owner string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, b := range s.boxes[owner] {
		if b.Agent {
			n++
		}
	}
	return n
}

// putBox stores b in the index in place of its previous version; the caller holds s.mu.
func (s *Service) putBox(b Box) {
	s.boxGen++
	list := slices.DeleteFunc(slices.Clone(s.boxes[b.Owner]), func(x Box) bool { return x.ID == b.ID })
	s.boxes[b.Owner] = slices.SortedFunc(slices.Values(append(list, b)), compareBoxes)
}

func checkBoxName(name string) error {
	if !boxNamePattern.MatchString(name) || strings.HasPrefix(name, reservedBoxPrefix) {
		return fmt.Errorf("%w: a box name is 1 to 32 lowercase letters, digits, - or _, starting with a letter or digit, and not starting with %s", ErrInvalid, reservedBoxPrefix)
	}
	return nil
}

func compareBoxes(a, b Box) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
}
