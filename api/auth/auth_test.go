package auth

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
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

type anyToken struct{}

func (anyToken) ByTokenSHA256(context.Context, string) (string, bool) { return "u-any", true }

type noShares struct{}

func (noShares) ShareBySecretSHA256(string) (Share, bool) { return Share{}, false }
