// Package auth resolves the bearer token on every call to a configured user.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/config"
)

const (
	// HeaderAuthorization carries `Bearer <user token>`.
	HeaderAuthorization = "authorization"

	bearerPrefix = "bearer "
)

type userKey struct{}

// Authenticator maps token hashes to user names.
type Authenticator struct {
	users map[string]string
}

// New indexes users by token hash.
func New(users []config.User) *Authenticator {
	a := &Authenticator{users: make(map[string]string, len(users))}
	for _, u := range users {
		a.users[u.TokenSHA256] = u.Name
	}
	return a
}

// Unary authenticates unary calls.
func (a *Authenticator) Unary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, err := a.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// Stream authenticates streaming calls, including those served by the unknown-service handler.
func (a *Authenticator) Stream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, err := a.authenticate(ss.Context())
	if err != nil {
		return err
	}
	return handler(srv, &authedStream{ServerStream: ss, ctx: ctx})
}

// Authenticate maps an Authorization header value to its user; the error is a gRPC status.
func (a *Authenticator) Authenticate(header string) (string, error) {
	if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", status.Error(codes.Unauthenticated, "missing bearer token")
	}
	sum := sha256.Sum256([]byte(header[len(bearerPrefix):]))
	user, ok := a.users[hex.EncodeToString(sum[:])]
	if !ok {
		return "", status.Error(codes.Unauthenticated, "invalid bearer token")
	}
	return user, nil
}

func (a *Authenticator) Identify(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(HeaderAuthorization)
	if len(values) != 1 {
		return ""
	}
	user, _ := a.Authenticate(values[0])
	return user
}

func (a *Authenticator) authenticate(ctx context.Context) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(HeaderAuthorization)
	if len(values) != 1 {
		return nil, status.Error(codes.Unauthenticated, "missing bearer token")
	}
	user, err := a.Authenticate(values[0])
	if err != nil {
		return nil, err
	}
	return WithUser(ctx, user), nil
}

type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authedStream) Context() context.Context { return s.ctx }

// User returns the authenticated user name, "" when ctx did not pass the interceptors.
func User(ctx context.Context) string {
	user, _ := ctx.Value(userKey{}).(string)
	return user
}

// WithUser returns ctx carrying user, as the interceptors do.
func WithUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, userKey{}, user)
}
