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

	"github.com/loomhand/cocoonbox/api/machines"
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
	// Flavor is the size every machine of the box takes, set when the box is created.
	machines.Flavor
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

// AddBox gives b.Owner the box b, ErrQuota past the owner's max boxes; an agent's box raises the owner's tenant cap by one.
func (s *Service) AddBox(ctx context.Context, b Box) (Box, error) {
	if err := checkBoxName(b.Name); err != nil {
		return Box{}, err
	}
	if _, err := s.record(b.Owner); err != nil {
		return Box{}, err
	}
	b.CreatedAt, b.Flavor = time.Now().Truncate(time.Microsecond), s.boxFlavor
	if err := s.store.insertBox(ctx, b); err != nil {
		return Box{}, err
	}
	unlock := s.editUser(b.Owner)
	s.putBox(b)
	unlock()
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
	defer s.editUser(owner)()
	i := slices.IndexFunc(s.boxes[owner], func(b Box) bool { return b.ID == id })
	if i < 0 {
		return Box{}, ErrNotFound
	}
	b := s.boxes[owner][i]
	b.Name = name
	s.putBox(b)
	return b, nil
}

// SetBoxFlavor makes flavor the size every later machine of owner's box id takes.
func (s *Service) SetBoxFlavor(ctx context.Context, owner, id string, flavor machines.Flavor) error {
	if !s.HasBox(owner, id) {
		return ErrNotFound
	}
	if err := s.store.setBoxFlavor(ctx, owner, id, flavor); err != nil {
		return err
	}
	defer s.editUser(owner)()
	i := slices.IndexFunc(s.boxes[owner], func(b Box) bool { return b.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	b := s.boxes[owner][i]
	b.Flavor = flavor
	s.putBox(b)
	return nil
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
	unlock := s.editUser(owner)
	s.boxes[owner] = slices.DeleteFunc(s.boxes[owner], func(b Box) bool { return b.ID == id })
	var shares []string
	for _, rec := range s.shares {
		if rec.box == id {
			shares = append(shares, rec.id)
		}
	}
	unlock()
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
	return s.tenants.PutTenant(ctx, owner, "", s.claimCap(r))
}

// claimCap is the tenant's max_claims: the owner's box quota, its agents' boxes up to max_agent_boxes, and one for the old machine a Rebase keeps.
func (s *Service) claimCap(r record) int {
	agent := s.agentBoxes(r.ID)
	if r.MaxAgentBoxes != nil {
		agent = min(agent, *r.MaxAgentBoxes)
	}
	boxes, _ := s.boxLimits(r)
	return boxes + agent + 1
}

// boxLimits is r's max boxes and max boxes cap, each the configured max_boxes while the row holds 0.
func (s *Service) boxLimits(r record) (boxes, limit int) {
	return cmp.Or(r.MaxBoxes, s.store.defaultMaxBoxes), cmp.Or(r.MaxBoxesCap, s.store.defaultMaxBoxes)
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

// putBox stores b in the index in place of its previous version; the caller holds b.Owner's stripe and s.mu.
func (s *Service) putBox(b Box) {
	list := append(slices.DeleteFunc(s.boxes[b.Owner], func(x Box) bool { return x.ID == b.ID }), b)
	slices.SortFunc(list, compareBoxes)
	s.boxes[b.Owner] = list
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
