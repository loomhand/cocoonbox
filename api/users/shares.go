package users

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/auth"
)

const (
	// DefaultShareTTL is a share's lifetime when its owner picks none.
	DefaultShareTTL = 24 * time.Hour
	// MaxShareTTL bounds a share's lifetime.
	MaxShareTTL = 30 * 24 * time.Hour

	minShareTTL      = time.Minute
	shareIDPrefix    = "sh-"
	shareSecretBytes = 16
)

// Share is a live share of a box as its owner sees it, for the owner only.
type Share struct {
	ID    string
	Owner string
	Box   string
	// Users are the users the share admits; empty admits anyone with the link.
	Users []ShareUser
	Write bool
	// Secret is empty for a share created before the api kept secrets.
	Secret    string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// ShareUser is a user a share names.
type ShareUser struct {
	ID   string
	Name string
}

// NewShare describes a share to create; a zero TTL is DefaultShareTTL, and no user names admit anyone with the link.
type NewShare struct {
	Owner     string
	Box       string
	TTL       time.Duration
	UserNames []string
	Write     bool
}

// CreateShare shares box ns.Box of ns.Owner and returns the share with its secret, which the api keeps sealed.
func (s *Service) CreateShare(ctx context.Context, ns NewShare) (Share, error) {
	ttl := cmp.Or(ns.TTL, DefaultShareTTL)
	if ttl < minShareTTL || ttl > MaxShareTTL {
		return Share{}, fmt.Errorf("%w: a share lasts from %s to %s", ErrInvalid, minShareTTL, MaxShareTTL)
	}
	if _, err := s.record(ns.Owner); err != nil {
		return Share{}, err
	}
	ids := make([]string, 0, len(ns.UserNames))
	for _, name := range ns.UserNames {
		u, err := s.ByName(name)
		if err != nil {
			return Share{}, fmt.Errorf("%w: no user is named %q", ErrInvalid, name)
		}
		if !slices.Contains(ids, u.ID) {
			ids = append(ids, u.ID)
		}
	}
	now := time.Now()
	secret := auth.SecretPrefix + randomHex(shareSecretBytes)
	id := shareIDPrefix + randomHex(idBytes)
	rec := &shareRecord{
		id: id, owner: ns.Owner, box: ns.Box, userIDs: ids, write: ns.Write,
		expiresAt: now.Add(ttl).Truncate(time.Millisecond), createdAt: now.Truncate(time.Millisecond), secret: secret, secretSHA256: hashHex(secret),
	}
	if err := s.store.insertShare(ctx, rec, s.seal.seal(secret, shareData(id, ns.Box))); err != nil {
		return Share{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putShare(rec)
	return s.view(rec), nil
}

// Shares returns owner's live shares, oldest first.
func (s *Service) Shares(owner string) []Share {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Share
	for _, rec := range s.shares {
		if rec.owner == owner {
			out = append(out, s.view(rec))
		}
	}
	slices.SortFunc(out, func(a, b Share) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// SetShareWrite turns write on or off for owner's share id; open MCP streams of the share learn it at once.
func (s *Service) SetShareWrite(ctx context.Context, owner, id string, write bool) (Share, error) {
	if _, err := s.shareBox(owner, id); err != nil {
		return Share{}, err
	}
	if err := s.store.setShareWrite(ctx, id, write); err != nil {
		return Share{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.shares[id]
	if !ok {
		return Share{}, ErrNotFound
	}
	if rec.write != write {
		rec.write = write
		close(rec.moded)
		rec.moded = make(chan struct{})
	}
	return s.view(rec), nil
}

// RegenerateShare gives owner's share id a new secret and ends every session of the old one.
func (s *Service) RegenerateShare(ctx context.Context, owner, id string) (Share, error) {
	box, err := s.shareBox(owner, id)
	if err != nil {
		return Share{}, err
	}
	secret := auth.SecretPrefix + randomHex(shareSecretBytes)
	if err := s.store.setShareSecret(ctx, id, hashHex(secret), s.seal.seal(secret, shareData(id, box))); err != nil {
		return Share{}, err
	}
	s.mu.Lock()
	rec, ok := s.shares[id]
	if !ok {
		s.mu.Unlock()
		return Share{}, ErrNotFound
	}
	delete(s.shareSecrets, rec.secretSHA256)
	rec.secret, rec.secretSHA256 = secret, hashHex(secret)
	s.shareSecrets[rec.secretSHA256] = id
	rec.cutStreams()
	view := s.view(rec)
	s.mu.Unlock()
	s.ended(ctx, view)
	return view, nil
}

// RevokeShare ends owner's share id and every session of it.
func (s *Service) RevokeShare(ctx context.Context, owner, id string) error {
	if _, err := s.shareBox(owner, id); err != nil {
		return err
	}
	if err := s.store.revokeShare(ctx, id); err != nil {
		return err
	}
	s.endShare(ctx, id)
	return nil
}

// ShareBySecretSHA256 returns the live share whose secret hashes to sum, while its owner is enabled.
func (s *Service) ShareBySecretSHA256(sum string) (auth.Share, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.shares[s.shareSecrets[sum]]
	if !ok {
		return auth.Share{}, false
	}
	if owner, ok := s.byID[rec.owner]; !ok || owner.Disabled {
		return auth.Share{}, false
	}
	return auth.Share{
		ID: rec.id, Owner: rec.owner, Box: rec.box, Users: rec.userIDs, Write: rec.write,
		ExpiresAt: rec.expiresAt, Cut: rec.cut, Moded: rec.moded,
	}, true
}

// OnShareEnd registers fn to run after a share is revoked, regenerated or expires, so the box can end its sessions.
func (s *Service) OnShareEnd(fn func(ctx context.Context, sh Share)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onShareEnd = append(s.onShareEnd, fn)
}

func (s *Service) loadShares(ctx context.Context) error {
	recs, err := s.store.liveShares(ctx)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.secretSealed == nil {
			continue
		}
		if rec.secret, err = s.seal.open(rec.secretSealed, shareData(rec.id, rec.box)); err != nil {
			log.WithFunc("users.Service.loadShares").Warnf(ctx, "share %s keeps no link: %v", rec.id, err)
		}
		rec.secretSealed = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range recs {
		s.putShare(rec)
	}
	return nil
}

// shareBox returns the box of owner's live share id.
func (s *Service) shareBox(owner, id string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.shares[id]
	if !ok || rec.owner != owner {
		return "", ErrNotFound
	}
	return rec.box, nil
}

// putShare indexes rec and arms its expiry; the caller holds s.mu.
func (s *Service) putShare(rec *shareRecord) {
	rec.cut, rec.moded = make(chan struct{}), make(chan struct{})
	s.shares[rec.id] = rec
	s.shareSecrets[rec.secretSHA256] = rec.id
	rec.timer = time.AfterFunc(time.Until(rec.expiresAt), func() { s.expireShare(context.Background(), rec.id) })
}

// expireShare forgets the secret of share id in its row, then ends the share.
func (s *Service) expireShare(ctx context.Context, id string) {
	if err := s.store.clearShareSecret(ctx, id); err != nil {
		log.WithFunc("users.Service.expireShare").Warnf(ctx, "clear the secret of expired share %s: %v", id, err)
	}
	s.endShare(ctx, id)
}

// endShare drops share id from the index, cuts its streams and runs the end hooks.
func (s *Service) endShare(ctx context.Context, id string) {
	s.mu.Lock()
	rec, ok := s.shares[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	s.dropShare(rec)
	view := s.view(rec)
	s.mu.Unlock()
	s.ended(ctx, view)
}

// dropShare removes rec from the index and cuts its streams; the caller holds s.mu.
func (s *Service) dropShare(rec *shareRecord) {
	rec.timer.Stop()
	delete(s.shares, rec.id)
	delete(s.shareSecrets, rec.secretSHA256)
	rec.cutStreams()
}

// cutOwner ends the streams of owner's shares, and drops the shares too when gone is set; the caller holds s.mu.
func (s *Service) cutOwner(owner string, gone bool) {
	for _, rec := range s.shares {
		switch {
		case rec.owner != owner:
		case gone:
			s.dropShare(rec)
		default:
			rec.cutStreams()
		}
	}
}

func (s *Service) ended(ctx context.Context, sh Share) {
	s.mu.RLock()
	hooks := slices.Clone(s.onShareEnd)
	s.mu.RUnlock()
	for _, fn := range hooks {
		fn(ctx, sh)
	}
}

// view is rec as its owner sees it; the caller holds s.mu.
func (s *Service) view(rec *shareRecord) Share {
	sh := Share{ID: rec.id, Owner: rec.owner, Box: rec.box, Write: rec.write, Secret: rec.secret, ExpiresAt: rec.expiresAt, CreatedAt: rec.createdAt}
	for _, id := range rec.userIDs {
		sh.Users = append(sh.Users, ShareUser{ID: id, Name: s.byID[id].Name})
	}
	return sh
}

// shareRecord is a live share in the index with the channels its streams watch.
type shareRecord struct {
	id           string
	owner        string
	box          string
	userIDs      []string
	write        bool
	expiresAt    time.Time
	createdAt    time.Time
	secret       string
	secretSHA256 string
	secretSealed []byte // the row's, until loadShares opens it
	cut          chan struct{}
	moded        chan struct{}
	timer        *time.Timer
}

// cutStreams ends the streams admitted so far; the caller holds the index lock.
func (r *shareRecord) cutStreams() {
	close(r.cut)
	r.cut = make(chan struct{})
}
