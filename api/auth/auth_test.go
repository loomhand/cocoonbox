package auth

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestAnInviteTokenIsNeverAUserTokenEvenWhenTheUsersWouldTakeIt(t *testing.T) {
	a := New(anyToken{}, noShares{})
	for _, header := range []string{"Bearer " + InviteTokenPrefix + "0123", "bearer " + InviteTokenPrefix, "BEARER " + InviteTokenPrefix + "x"} {
		if id, err := a.Authenticate(t.Context(), EntryGRPC, header); status.Code(err) != codes.Unauthenticated || id.User != "" {
			t.Errorf("Authenticate(%q) = %+v, %v, want Unauthenticated", header, id, err)
		}
		if id, err := a.Guest(t.Context(), EntryMCP, header[len(bearerPrefix):]); status.Code(err) != codes.Unauthenticated || id.User != "" {
			t.Errorf("Guest(%q) = %+v, %v, want Unauthenticated", header, id, err)
		}
	}
	if id, err := a.Authenticate(t.Context(), EntryGRPC, "Bearer cb_0123"); err != nil || id != (Identity{User: "u-any", Credential: TokenCredential("cb_0123")}) {
		t.Errorf("a user token = %+v, %v, want u-any with the token's credential", id, err)
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

func TestOnlyTheOperatorSocketMarksAnOperatorCall(t *testing.T) {
	a := New(anyToken{}, noShares{})
	operator := func(ctx context.Context, _ any) (any, error) { return Operator(ctx), nil }
	got, err := a.Unary(metadata.NewIncomingContext(t.Context(), metadata.Pairs(HeaderAuthorization, "Bearer cb_0123", HeaderOperatorUser, "u-any")), nil, &grpc.UnaryServerInfo{}, operator)
	if err != nil || got != false {
		t.Errorf("a user call with the operator header: operator %v, %v", got, err)
	}
	got, err = OperatorUnary(metadata.NewIncomingContext(t.Context(), metadata.Pairs(HeaderOperatorUser, "u-any")), nil, &grpc.UnaryServerInfo{}, operator)
	if err != nil || got != true {
		t.Errorf("an operator socket call: operator %v, %v", got, err)
	}
}

func TestAnIdentityProviderNamesTheCredentialItsCallsEndWith(t *testing.T) {
	a := WithIdentities(provider{"ext-alice": {User: "alice", Credential: "idp:session-1"}}, noShares{})
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs(HeaderAuthorization, "Bearer ext-alice"))
	var user, credential string
	_, err := a.Unary(ctx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, _ any) (any, error) {
		user, credential = User(ctx), Credential(ctx)
		return nil, nil
	})
	if err != nil || user != "alice" || credential != "idp:session-1" {
		t.Errorf("Unary saw user %q credential %q, %v", user, credential, err)
	}
	if id, err := a.Authenticate(t.Context(), EntryGRPC, "Bearer ext-alice"); err != nil || id != (Identity{User: "alice", Credential: "idp:session-1"}) {
		t.Errorf("Authenticate = %+v, %v", id, err)
	}
	if id, err := a.Guest(t.Context(), EntryMCP, "ext-alice"); err != nil || id.Credential != "idp:session-1" {
		t.Errorf("Guest = %+v, %v", id, err)
	}
}

func TestIdentifyNamesTheCallerOrNobody(t *testing.T) {
	a := WithIdentities(provider{"ext-alice": {User: "alice", Credential: "idp:session-1"}}, noShares{})
	for header, want := range map[string]string{"Bearer ext-alice": "alice", "Bearer ext-nobody": "", "": ""} {
		md := metadata.MD{}
		if header != "" {
			md = metadata.Pairs(HeaderAuthorization, header)
		}
		if got := a.Identify(metadata.NewIncomingContext(t.Context(), md)); got != want {
			t.Errorf("Identify(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestAProviderThatCannotAnswerIsUnavailableNotUnauthenticated(t *testing.T) {
	a := WithIdentities(provider{}, noShares{})
	if _, err := a.Authenticate(t.Context(), EntryGRPC, "Bearer ext-down"); status.Code(err) != codes.Unavailable {
		t.Errorf("a provider outage = %v, want Unavailable", err)
	}
	if _, err := a.Authenticate(t.Context(), EntryGRPC, "Bearer ext-nobody"); status.Code(err) != codes.Unauthenticated {
		t.Errorf("an unknown token = %v, want Unauthenticated", err)
	}
}

func TestCocoonboxsOwnTokensNeverReachTheOutsideProvider(t *testing.T) {
	outside := &recorder{}
	a := WithIdentities(Outside(Tokens(anyToken{}), outside), noShares{})
	for _, token := range []string{"cb_0123", "cba_0123", "cbs_0123", "cbr_0123", "cbx_0123"} {
		if id, err := a.Authenticate(t.Context(), EntryGRPC, "Bearer "+token); err != nil || id.User != "u-any" {
			t.Errorf("Authenticate(%s) = %+v, %v, want the local user", token, id, err)
		}
	}
	if len(outside.seen) != 0 {
		t.Errorf("the outside provider saw %v", outside.seen)
	}
	if id, err := a.Authenticate(t.Context(), EntryGRPC, "Bearer sapi_0123"); err != nil || id.User != "outside" || len(outside.seen) != 1 {
		t.Errorf("an outside token = %+v, %v; seen %v", id, err, outside.seen)
	}
}

func TestAShareSecretIsNoOutsideProvidersBusinessEvenWhileItIsDown(t *testing.T) {
	a := WithIdentities(Outside(Tokens(noToken{}), provider{}), noShares{})
	if _, err := a.Authenticate(t.Context(), EntryGRPC, "Bearer cbs_0123"); status.Code(err) != codes.Unauthenticated {
		t.Errorf("a share secret with the provider down = %v, want Unauthenticated, so the share path takes it", err)
	}
	if _, err := a.Authenticate(t.Context(), EntryGRPC, "Bearer ext-down"); status.Code(err) != codes.Unavailable {
		t.Errorf("an outside token with the provider down = %v, want Unavailable", err)
	}
}

func TestAProviderLearnsTheEntryAndMayKeepATokenOutOfOne(t *testing.T) {
	a := WithIdentities(provider{"ext-mcp": {User: "alice", Credential: "idp:session-1"}}, noShares{})
	if id, err := a.Authenticate(t.Context(), EntryMCP, "Bearer ext-mcp"); err != nil || id.User != "alice" {
		t.Errorf("an MCP token at MCP = %+v, %v", id, err)
	}
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs(HeaderAuthorization, "Bearer ext-mcp"))
	if _, err := a.Unary(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { return nil, nil }); status.Code(err) != codes.PermissionDenied {
		t.Errorf("an MCP token on gRPC = %v, want PermissionDenied", err)
	}
}

func TestAnOutsideCredentialIsWatchedWhileACallHoldsIt(t *testing.T) {
	outside := &recorder{}
	a := WithIdentities(Outside(Tokens(anyToken{}), outside), noShares{})
	_, stop := a.Bind(t.Context(), "outside", "outside:sapi_0123")
	if len(outside.watched) != 1 || len(outside.released) != 0 {
		t.Fatalf("watched %v, released %v while the call runs", outside.watched, outside.released)
	}
	stop()
	if len(outside.released) != 1 || outside.released[0] != "outside:sapi_0123" {
		t.Errorf("released %v after the call", outside.released)
	}
}

type recorder struct{ seen, watched, released []string }

func (r *recorder) Resolve(_ context.Context, token string, _ Entry) (Identity, error) {
	r.seen = append(r.seen, token)
	return Identity{User: "outside", Credential: "outside:" + token}, nil
}

func (r *recorder) Watch(credential string) func() {
	r.watched = append(r.watched, credential)
	return func() { r.released = append(r.released, credential) }
}

type noToken struct{ anyToken }

func (noToken) ByTokenSHA256(context.Context, string) (string, bool) { return "", false }

type provider map[string]Identity

func (p provider) Resolve(_ context.Context, token string, entry Entry) (Identity, error) {
	if token == "ext-mcp" && entry != EntryMCP {
		return Identity{}, ErrNotAllowed
	}
	if token == "ext-down" {
		return Identity{}, errors.New("identity provider unreachable")
	}
	if id, ok := p[token]; ok {
		return id, nil
	}
	return Identity{}, ErrUnknownToken
}

func (provider) Watch(string) func() { return func() {} }

func (provider) Lifetime(string, string) (context.Context, func()) {
	return context.Background(), func() {}
}

type anyToken struct{}

func (anyToken) ByTokenSHA256(context.Context, string) (string, bool) { return "u-any", true }

func (anyToken) Lifetime(string, string) (context.Context, func()) {
	return context.Background(), func() {}
}

type noShares struct{}

func (noShares) ShareBySecretSHA256(string) (Share, bool) { return Share{}, false }
