// Package auth resolves the bearer token on every call to a user id, or to a share of a box.
package auth

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/utils"
)

const (
	// HeaderAuthorization carries `Bearer <user token>`, or `Bearer <share secret>` on MCP.
	HeaderAuthorization = "authorization"
	// HeaderGuestToken carries the caller's own user token next to the secret of a share that names its users.
	HeaderGuestToken = "x-cocoonbox-guest-token" //nolint:gosec // a header name, not a credential
	// HeaderOperatorUser names the user an operator call acts as; only the operator socket reads it.
	HeaderOperatorUser = "x-cocoonbox-operator-user"
	// SecretPrefix starts every share secret; a bearer is tried as a share secret only when no user token matches it.
	SecretPrefix = "cbs_"
	// InviteTokenPrefix starts every invite-scoped token, which is never a user token.
	InviteTokenPrefix = "cbit_"

	// EntryGRPC and EntryMCP name the door a bearer came through, for a provider that admits a token at some doors only.
	EntryGRPC Entry = "grpc"
	EntryMCP  Entry = "mcp"

	bearerPrefix = "bearer "
)

var (
	// ErrUnknownToken is the Identities answer for a token that names no enabled user; any other error is the provider failing.
	ErrUnknownToken = errors.New("unknown token")
	// ErrNotAllowed is the Identities answer for a valid token its owner does not let through this entry.
	ErrNotAllowed = errors.New("token not allowed at this entry")

	// ownToken matches every secret cocoonbox issues: cb_, cba_, cbs_, cbit_ and the rest.
	ownToken = regexp.MustCompile(`^cb[a-z]*_`)
)

// Entry is the door of the api a bearer came through.
type Entry string

type (
	userKey       struct{}
	credentialKey struct{}
	operatorKey   struct{}
)

// Identities resolves a bearer token to the enabled user it names, and gives the context that ends when that user is disabled or deleted or the credential ends.
type Identities interface {
	Resolve(ctx context.Context, token string, entry Entry) (Identity, error)
	Lifetime(user, credential string) (context.Context, func())
}

// Identity is the user a bearer token names and the credential the calls it admits are bound to.
type Identity struct {
	User       string
	Credential string
}

// Users resolves the sha256 of an API token, in lowercase hex, to an enabled user id, and gives the context that ends when the user is disabled or deleted or the credential ends.
type Users interface {
	ByTokenSHA256(ctx context.Context, sum string) (string, bool)
	Lifetime(user, credential string) (context.Context, func())
}

// Tokens is the Identities of cocoonbox's own API and device tokens, each bound to its sha256.
func Tokens(users Users) Identities {
	return tokens{users}
}

type tokens struct {
	Users
}

func (t tokens) Resolve(ctx context.Context, token string, _ Entry) (Identity, error) {
	credential := TokenCredential(token)
	user, ok := t.ByTokenSHA256(ctx, credential)
	if !ok {
		return Identity{}, ErrUnknownToken
	}
	return Identity{User: user, Credential: credential}, nil
}

// Resolver resolves a bearer token as Identities does, for the users of another system, and keeps checking a credential it named while calls hold it.
type Resolver interface {
	Resolve(ctx context.Context, token string, entry Entry) (Identity, error)
	Watch(credential string) (release func())
}

// Outside resolves the tokens cocoonbox does not issue through outside; its own stay with local, which also gives every call's lifetime.
func Outside(local Identities, outside Resolver) Identities {
	return split{Identities: local, outside: outside}
}

type split struct {
	Identities
	outside Resolver
}

func (s split) Resolve(ctx context.Context, token string, entry Entry) (Identity, error) {
	if ownToken.MatchString(token) {
		return s.Identities.Resolve(ctx, token, entry)
	}
	return s.outside.Resolve(ctx, token, entry)
}

func (s split) Lifetime(user, credential string) (context.Context, func()) {
	life, release := s.Identities.Lifetime(user, credential)
	unwatch := s.outside.Watch(credential)
	return life, func() {
		unwatch()
		release()
	}
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
	identities Identities
	shares     Shares
}

// New authenticates cocoonbox's own tokens against users, and share secrets against shares.
func New(users Users, shares Shares) *Authenticator {
	return WithIdentities(Tokens(users), shares)
}

// WithIdentities authenticates bearer tokens against identities, and share secrets against shares.
func WithIdentities(identities Identities, shares Shares) *Authenticator {
	return &Authenticator{identities: identities, shares: shares}
}

// Admit admits a share secret for box, or for the share's own box when box is empty; guest is the caller's user id or empty.
func (a *Authenticator) Admit(secret, box, guest string) (Grant, error) {
	sh, ok := a.shares.ShareBySecretSHA256(utils.HashHex(secret))
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

// Guest maps a guest token header value to its identity, the zero Identity for no token; the error is a gRPC status.
func (a *Authenticator) Guest(ctx context.Context, entry Entry, token string) (Identity, error) {
	if token == "" {
		return Identity{}, nil
	}
	return a.resolve(ctx, entry, token, "invalid guest token")
}

func (a *Authenticator) Unary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, err := a.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// Stream authenticates streaming calls, including those served by the unknown-service handler, and ends each when its user is disabled or deleted or its token revoked.
func (a *Authenticator) Stream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, err := a.authenticate(ss.Context())
	if err != nil {
		return err
	}
	ctx, stop := a.Bind(ctx, User(ctx), Credential(ctx))
	defer stop()
	return handler(srv, &authedStream{ServerStream: ss, ctx: ctx})
}

// Authenticate maps an Authorization header value that came through entry to its identity; the error is a gRPC status.
func (a *Authenticator) Authenticate(ctx context.Context, entry Entry, header string) (Identity, error) {
	token, ok := Bearer(header)
	if !ok {
		return Identity{}, status.Error(codes.Unauthenticated, "missing bearer token")
	}
	return a.resolve(ctx, entry, token, "invalid bearer token")
}

func (a *Authenticator) Bind(ctx context.Context, user, credential string) (context.Context, context.CancelFunc) {
	life, release := a.identities.Lifetime(user, credential)
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(life, cancel)
	return ctx, func() {
		stop()
		cancel()
		release()
	}
}

func (a *Authenticator) Identify(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(HeaderAuthorization)
	if len(values) != 1 {
		return ""
	}
	id, _ := a.Authenticate(ctx, EntryGRPC, values[0])
	return id.User
}

func (a *Authenticator) resolve(ctx context.Context, entry Entry, token, refusal string) (Identity, error) {
	if strings.HasPrefix(token, InviteTokenPrefix) {
		return Identity{}, status.Error(codes.Unauthenticated, refusal)
	}
	id, err := a.identities.Resolve(ctx, token, entry)
	switch {
	case errors.Is(err, ErrUnknownToken):
		return Identity{}, status.Error(codes.Unauthenticated, refusal)
	case errors.Is(err, ErrNotAllowed):
		return Identity{}, status.Errorf(codes.PermissionDenied, "this token may not use %s", entry)
	case err != nil:
		log.WithFunc("auth.Authenticator.resolve").Error(ctx, err, "resolve a bearer token")
		return Identity{}, status.Error(codes.Unavailable, "checking the bearer token failed")
	}
	return id, nil
}

func (a *Authenticator) authenticate(ctx context.Context) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(HeaderAuthorization)
	if len(values) != 1 {
		return nil, status.Error(codes.Unauthenticated, "missing bearer token")
	}
	id, err := a.Authenticate(ctx, EntryGRPC, values[0])
	if err != nil {
		return nil, err
	}
	return context.WithValue(WithUser(ctx, id.User), credentialKey{}, id.Credential), nil
}

type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authedStream) Context() context.Context { return s.ctx }

// ShareSecret returns the share secret an Authorization header carries.
func ShareSecret(header string) (string, bool) {
	secret, ok := Bearer(header)
	return secret, ok && strings.HasPrefix(secret, SecretPrefix)
}

func Bearer(header string) (string, bool) {
	if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	return header[len(bearerPrefix):], true
}

// TokenCredential names a user or device token's credential for Bind: its sha256 in lowercase hex.
func TokenCredential(token string) string {
	return utils.HashHex(token)
}

// Credential returns the credential the interceptors admitted, "" when ctx did not pass them.
func Credential(ctx context.Context) string {
	credential, _ := ctx.Value(credentialKey{}).(string)
	return credential
}

// User returns the authenticated user id, "" when ctx did not pass the interceptors.
func User(ctx context.Context) string {
	user, _ := ctx.Value(userKey{}).(string)
	return user
}

func Caller(ctx context.Context) (string, error) {
	if user := User(ctx); user != "" {
		return user, nil
	}
	return "", status.Error(codes.Unauthenticated, "unauthenticated")
}

// WithUser returns ctx carrying user, as the interceptors do.
func WithUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, userKey{}, user)
}

// Operator reports whether ctx is a call of the operator socket.
func Operator(ctx context.Context) bool {
	operator, _ := ctx.Value(operatorKey{}).(bool)
	return operator
}

// OperatorUnary admits unary calls as the user in HeaderOperatorUser; the operator socket's file mode is the access check.
func OperatorUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, err := operatorUser(ctx)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// OperatorStream admits streaming calls as the user in HeaderOperatorUser.
func OperatorStream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, err := operatorUser(ss.Context())
	if err != nil {
		return err
	}
	return handler(srv, &authedStream{ServerStream: ss, ctx: ctx})
}

func operatorUser(ctx context.Context) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(HeaderOperatorUser)
	if len(values) != 1 || values[0] == "" {
		return nil, status.Error(codes.Unauthenticated, "missing operator user")
	}
	return context.WithValue(WithUser(ctx, values[0]), operatorKey{}, true), nil
}
