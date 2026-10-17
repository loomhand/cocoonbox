package auth

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestAnInviteTokenIsNeverAUserTokenEvenWhenTheUsersWouldTakeIt(t *testing.T) {
	a := New(anyToken{}, noShares{})
	for _, header := range []string{"Bearer " + InviteTokenPrefix + "0123", "bearer " + InviteTokenPrefix, "BEARER " + InviteTokenPrefix + "x"} {
		if user, err := a.Authenticate(t.Context(), header); status.Code(err) != codes.Unauthenticated || user != "" {
			t.Errorf("Authenticate(%q) = %q, %v, want Unauthenticated", header, user, err)
		}
		if user, err := a.Guest(t.Context(), header[len(bearerPrefix):]); status.Code(err) != codes.Unauthenticated || user != "" {
			t.Errorf("Guest(%q) = %q, %v, want Unauthenticated", header, user, err)
		}
	}
	if user, err := a.Authenticate(t.Context(), "Bearer cb_0123"); err != nil || user != "u-any" {
		t.Errorf("a user token = %q, %v, want u-any", user, err)
	}
}

func TestTheInterceptorsCarryTheTokensCredential(t *testing.T) {
	a := New(anyToken{}, noShares{})
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs(HeaderAuthorization, "Bearer cb_0123"))
	var user, credential string
	_, err := a.Unary(ctx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, _ any) (any, error) {
		user, credential = User(ctx), Credential(ctx)
		return nil, nil
	})
	if err != nil || user != "u-any" || credential != TokenCredential("cb_0123") || len(credential) != 64 {
		t.Errorf("Unary saw user %q credential %q, %v", user, credential, err)
	}
}

type anyToken struct{}

func (anyToken) ByTokenSHA256(context.Context, string) (string, bool) { return "u-any", true }

func (anyToken) Lifetime(string, string) context.Context { return context.Background() }

type noShares struct{}

func (noShares) ShareBySecretSHA256(string) (Share, bool) { return Share{}, false }
