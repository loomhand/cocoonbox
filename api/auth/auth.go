// Package auth resolves the bearer token on every call to a user id, or to a share of a box.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	// HeaderAuthorization carries `Bearer <user token>`, or `Bearer <share secret>` on MCP.
	HeaderAuthorization = "authorization"
	// HeaderGuestToken carries the caller's own user token next to the secret of a share that names its users.
	HeaderGuestToken = "x-cocoonbox-guest-token" //nolint:gosec // a header name, not a credential
	// SecretPrefix starts every share secret; a bearer is tried as a share secret only when no user token matches it.
	SecretPrefix = "cbs_"

	bearerPrefix = "bearer "
)

type userKey struct{}

// Users resolves the sha256 of an API token, in lowercase hex, to an enabled user id.
type Users interface {
	ByTokenSHA256(sum string) (string, bool)
}

// Shares resolves the sha256 of a share secret, in lowercase hex, to a live share of an enabled owner.
type Shares interface {
	ShareBySecretSHA256(sum string) (Share, bool)
}

// Share is a live share as the edge admits it.
type Share struct {
	ID    string
	Owner string
	Box   string
	// Users are the user ids the share admits; empty admits anyone with the link.
	Users     []string
	Write     bool
	ExpiresAt time.Time
	// Cut closes when the streams admitted so far must end: revoke, regenerate, expiry, or the owner disabled or deleted.
	Cut <-chan struct{}
	// Moded closes when the owner switches write.
	Moded <-chan struct{}
}

// Grant is one admitted share call: the share and the guest's user id, empty for an open share.
type Grant struct {
	Share
	Guest string
}

// Principal names the caller in the box credential: share:<id>:<read|write>[:<guest>].
func (g Grant) Principal() string {
	mode := "read"
	if g.Write {
		mode = "write"
	}
	p := "share:" + g.ID + ":" + mode
	if g.Guest != "" {
		p += ":" + g.Guest
	}
	return p
}

// Authenticator maps bearer tokens to user ids and share secrets to shares.
type Authenticator struct {
	users  Users
	shares Shares
}

// New authenticates against users and shares.
func New(users Users, shares Shares) *Authenticator {
	return &Authenticator{users: users, shares: shares}
}

// ShareSecret returns the share secret an Authorization header carries.
func ShareSecret(header string) (string, bool) {
	if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	secret := header[len(bearerPrefix):]
	return secret, strings.HasPrefix(secret, SecretPrefix)
}

// Admit admits a share secret for box, or for the share's own box when box is empty; guest is the caller's user id or empty.
func (a *Authenticator) Admit(secret, box, guest string) (Grant, error) {
	sum := sha256.Sum256([]byte(secret))
	sh, ok := a.shares.ShareBySecretSHA256(hex.EncodeToString(sum[:]))
	switch {
	case !ok || !time.Now().Before(sh.ExpiresAt):
		return Grant{}, status.Error(codes.PermissionDenied, "the share link is revoked, regenerated or expired")
	case box != "" && box != sh.Box:
		return Grant{}, status.Error(codes.PermissionDenied, "the share link is for another box")
	case len(sh.Users) == 0:
		return Grant{Share: sh}, nil
	case guest == "":
		return Grant{}, status.Error(codes.Unauthenticated, "this share names its users: send your own cocoonbox token too")
	case !slices.Contains(sh.Users, guest):
		return Grant{}, status.Error(codes.PermissionDenied, "this share does not name you")
	}
	return Grant{Share: sh, Guest: guest}, nil
}

// Guest maps a guest token header value to its user; the error is a gRPC status.
func (a *Authenticator) Guest(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	user, ok := a.byToken(token)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "invalid guest token")
	}
	return user, nil
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
	user, ok := a.byToken(header[len(bearerPrefix):])
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

func (a *Authenticator) byToken(token string) (string, bool) {
	sum := sha256.Sum256([]byte(token))
	return a.users.ByTokenSHA256(hex.EncodeToString(sum[:]))
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

// User returns the authenticated user id, "" when ctx did not pass the interceptors.
func User(ctx context.Context) string {
	user, _ := ctx.Value(userKey{}).(string)
	return user
}

// WithUser returns ctx carrying user, as the interceptors do.
func WithUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, userKey{}, user)
}
