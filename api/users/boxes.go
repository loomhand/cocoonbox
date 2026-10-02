package users

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// FirstBoxName names the box every user starts with.
	FirstBoxName = "main"

	reservedBoxPrefix = "cb-"
)

var boxNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Box is one of a user's boxes: its id names its machines and its state, its name is unique among the owner's boxes.
type Box struct {
	ID        string
	Owner     string
	Name      string
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

// AddBox gives b.Owner the box b, refused with ErrQuota once the owner has max boxes.
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
	defer s.mu.Unlock()
	s.putBox(b)
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

// RemoveBox removes owner's box id and revokes its share, which cuts the share's sessions.
func (s *Service) RemoveBox(ctx context.Context, owner, id string) error {
	if !s.HasBox(owner, id) {
		return ErrNotFound
	}
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
	return nil
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
