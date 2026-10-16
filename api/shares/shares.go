// Package shares serves cocoonbox.control.v1.Shares: the owner's calls on the shares of their boxes.
package shares

import (
	"context"
	"errors"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/boxes"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/users"
)

// Store keeps the boxes and their shares; users.Service is the production one.
type Store interface {
	HasBox(owner, id string) bool
	CreateShare(ctx context.Context, ns users.NewShare) (users.Share, error)
	Shares(owner string) []users.Share
	SetShareWrite(ctx context.Context, owner, id string, write bool) (users.Share, error)
	RegenerateShare(ctx context.Context, owner, id string) (users.Share, error)
	RevokeShare(ctx context.Context, owner, id string) error
}

// Server implements controlv1.SharesServer for the authenticated caller's own boxes.
type Server struct {
	controlv1.UnimplementedSharesServer

	store Store
}

// New serves the shares in store.
func New(store Store) *Server {
	return &Server{store: store}
}

func (s *Server) Create(ctx context.Context, req *controlv1.CreateShareRequest) (*controlv1.ShareSecret, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	if !s.store.HasBox(user, req.GetBoxId()) {
		return nil, boxes.ErrNoBox
	}
	sh, err := s.store.CreateShare(ctx, users.NewShare{
		Owner: user, Box: req.GetBoxId(), TTL: time.Duration(req.GetTtlSeconds()) * time.Second,
		UserNames: req.GetUserNames(), Write: req.GetWrite(),
	})
	if err != nil {
		return nil, storeError(ctx, err, "create share")
	}
	log.WithFunc("shares.Server.Create").Infof(ctx, "user %s shared box %s as %s: %d named users, write %t, until %s",
		user, sh.Box, sh.ID, len(sh.Users), sh.Write, sh.ExpiresAt.UTC().Format(time.RFC3339))
	return &controlv1.ShareSecret{Share: toShare(sh), Secret: sh.Secret}, nil
}

func (s *Server) List(ctx context.Context, _ *controlv1.ListSharesRequest) (*controlv1.ListSharesResponse, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	resp := &controlv1.ListSharesResponse{}
	for _, sh := range s.store.Shares(user) {
		resp.Shares = append(resp.Shares, toShare(sh))
	}
	return resp, nil
}

func (s *Server) SetWrite(ctx context.Context, req *controlv1.SetShareWriteRequest) (*controlv1.Share, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	sh, err := s.store.SetShareWrite(ctx, user, req.GetShareId(), req.GetWrite())
	if err != nil {
		return nil, storeError(ctx, err, "set share write")
	}
	log.WithFunc("shares.Server.SetWrite").Infof(ctx, "user %s set write of share %s to %t", user, sh.ID, sh.Write)
	return toShare(sh), nil
}

func (s *Server) Regenerate(ctx context.Context, ref *controlv1.ShareRef) (*controlv1.ShareSecret, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	sh, err := s.store.RegenerateShare(ctx, user, ref.GetShareId())
	if err != nil {
		return nil, storeError(ctx, err, "regenerate share")
	}
	log.WithFunc("shares.Server.Regenerate").Infof(ctx, "user %s regenerated share %s", user, sh.ID)
	return &controlv1.ShareSecret{Share: toShare(sh), Secret: sh.Secret}, nil
}

func (s *Server) Revoke(ctx context.Context, ref *controlv1.ShareRef) (*controlv1.RevokeShareResponse, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.store.RevokeShare(ctx, user, ref.GetShareId()); err != nil {
		return nil, storeError(ctx, err, "revoke share")
	}
	log.WithFunc("shares.Server.Revoke").Infof(ctx, "user %s revoked share %s", user, ref.GetShareId())
	return &controlv1.RevokeShareResponse{}, nil
}

func toShare(sh users.Share) *controlv1.Share {
	out := &controlv1.Share{Id: sh.ID, BoxId: sh.Box, Write: sh.Write, Secret: sh.Secret, ExpiresAtMs: sh.ExpiresAt.UnixMilli(), CreatedAtMs: sh.CreatedAt.UnixMilli()}
	for _, u := range sh.Users {
		out.Users = append(out.Users, &controlv1.ShareUser{Id: u.ID, Name: u.Name})
	}
	return out
}

func storeError(ctx context.Context, err error, action string) error {
	switch {
	case errors.Is(err, users.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, users.ErrExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, users.ErrNotFound):
		return status.Error(codes.NotFound, "share not found")
	default:
		log.WithFunc("shares.storeError").Error(ctx, err, action)
		return status.Errorf(codes.Unavailable, "%s failed", action)
	}
}
