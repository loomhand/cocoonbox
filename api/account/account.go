// Package account serves cocoonbox.control.v1.Account: the caller's own sign-ins, which the caller lists and revokes.
package account

import (
	"context"
	"errors"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/users"
)

// Users keeps the personal and device tokens; users.Service is the production one.
type Users interface {
	Devices(ctx context.Context, id string) ([]users.Device, error)
	TokenDevice(sum string) string
	RevokeDevice(ctx context.Context, id, device string) error
	RevokeToken(ctx context.Context, id string) error
}

// Server implements controlv1.AccountServer for the authenticated caller.
type Server struct {
	controlv1.UnimplementedAccountServer

	users Users
}

// New serves the sign-ins in us.
func New(us Users) *Server {
	return &Server{users: us}
}

func (s *Server) ListDevices(ctx context.Context, _ *controlv1.ListDevicesRequest) (*controlv1.ListDevicesResponse, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	devices, err := s.users.Devices(ctx, user)
	if err != nil {
		return nil, usersError(ctx, err, "list devices")
	}
	current := s.users.TokenDevice(auth.Credential(ctx))
	resp := &controlv1.ListDevicesResponse{Devices: make([]*controlv1.Device, 0, len(devices))}
	for _, d := range devices {
		out := &controlv1.Device{Id: d.ID, Client: d.Client, Label: d.Label, CreatedAtMs: d.CreatedAt.UnixMilli(), Current: d.ID == current}
		if d.UsedAt != nil {
			out.UsedAtMs = d.UsedAt.UnixMilli()
		}
		resp.Devices = append(resp.Devices, out)
	}
	return resp, nil
}

func (s *Server) RevokeDevice(ctx context.Context, req *controlv1.RevokeDeviceRequest) (*controlv1.RevokeDeviceResponse, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	switch err := s.users.RevokeDevice(ctx, user, req.GetDeviceId()); {
	case errors.Is(err, users.ErrNotFound):
		return nil, status.Error(codes.NotFound, "device not found")
	case err != nil:
		return nil, usersError(ctx, err, "revoke device")
	}
	log.WithFunc("account.Server.RevokeDevice").Infof(ctx, "user %s revoked device %s", user, req.GetDeviceId())
	return &controlv1.RevokeDeviceResponse{}, nil
}

func (s *Server) RevokeToken(ctx context.Context, _ *controlv1.RevokeTokenRequest) (*controlv1.RevokeTokenResponse, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.users.RevokeToken(ctx, user); err != nil {
		return nil, usersError(ctx, err, "revoke token")
	}
	log.WithFunc("account.Server.RevokeToken").Infof(ctx, "user %s revoked the personal token", user)
	return &controlv1.RevokeTokenResponse{}, nil
}

func usersError(ctx context.Context, err error, action string) error {
	log.WithFunc("account.usersError").Error(ctx, err, action)
	return status.Errorf(codes.Internal, "%s failed", action)
}
