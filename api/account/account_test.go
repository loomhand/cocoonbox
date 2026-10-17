package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/users"
)

const (
	cliToken      = "cb_cli"
	appToken      = "cb_app"
	personalToken = "cb_personal"
)

func TestListDevicesMarksTheCallersOwnDevice(t *testing.T) {
	us := newFakeUsers()
	srv := New(us)
	if _, err := srv.ListDevices(t.Context(), &controlv1.ListDevicesRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("ListDevices without a user: %v", err)
	}
	got, err := srv.ListDevices(as(t, us, appToken), &controlv1.ListDevicesRequest{})
	if err != nil || len(got.GetDevices()) != 2 {
		t.Fatalf("ListDevices = %v, %v", got, err)
	}
	app, cli := got.GetDevices()[0], got.GetDevices()[1]
	if !app.GetCurrent() || cli.GetCurrent() || app.GetUsedAtMs() == 0 || cli.GetUsedAtMs() != 0 || cli.GetLabel() != "laptop" {
		t.Errorf("devices = %v, want the app's current and used, the CLI's never used", got.GetDevices())
	}
	if got, err := srv.ListDevices(as(t, us, personalToken), &controlv1.ListDevicesRequest{}); err != nil || got.GetDevices()[0].GetCurrent() || got.GetDevices()[1].GetCurrent() {
		t.Errorf("the personal token's ListDevices = %v, %v, want no current device", got, err)
	}
}

func TestRevokesActOnTheCallerOnly(t *testing.T) {
	us := newFakeUsers()
	srv := New(us)
	alice := as(t, us, cliToken)
	if _, err := srv.RevokeDevice(alice, &controlv1.RevokeDeviceRequest{DeviceId: "dev_bob"}); status.Code(err) != codes.NotFound {
		t.Errorf("RevokeDevice of another user's device: %v", err)
	}
	if _, err := srv.RevokeDevice(alice, &controlv1.RevokeDeviceRequest{DeviceId: "dev_app"}); err != nil || us.revoked != "alice/dev_app" {
		t.Errorf("RevokeDevice = %v, revoked %q", err, us.revoked)
	}
	if _, err := srv.RevokeToken(alice, &controlv1.RevokeTokenRequest{}); err != nil || us.tokenRevoked != "alice" {
		t.Errorf("RevokeToken = %v, revoked %q", err, us.tokenRevoked)
	}
	us.err = errors.New("read user_tokens: connection refused")
	for _, err := range []error{
		func() error { _, err := srv.ListDevices(alice, &controlv1.ListDevicesRequest{}); return err }(),
		func() error { _, err := srv.RevokeToken(alice, &controlv1.RevokeTokenRequest{}); return err }(),
	} {
		if st := status.Convert(err); st.Code() != codes.Internal || st.Message() == us.err.Error() {
			t.Errorf("a store failure = %v, want Internal without its text", err)
		}
	}
}

func as(t *testing.T, us *fakeUsers, token string) context.Context {
	t.Helper()
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs(auth.HeaderAuthorization, "Bearer "+token))
	var out context.Context
	_, err := auth.New(us, noShares{}).Unary(ctx, nil, nil, func(ctx context.Context, _ any) (any, error) {
		out = ctx
		return nil, nil
	})
	if err != nil {
		t.Fatalf("authenticate %q: %v", token, err)
	}
	return out
}

type fakeUsers struct {
	devices      map[string]string
	used         time.Time
	revoked      string
	tokenRevoked string
	err          error
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{devices: map[string]string{auth.TokenCredential(cliToken): "dev_cli", auth.TokenCredential(appToken): "dev_app"}, used: time.Now()}
}

func (f *fakeUsers) ByTokenSHA256(_ context.Context, sum string) (string, bool) {
	if _, ok := f.devices[sum]; ok || sum == auth.TokenCredential(personalToken) {
		return "alice", true
	}
	return "", false
}

func (f *fakeUsers) Lifetime(string, string) context.Context { return context.Background() }

func (f *fakeUsers) Devices(_ context.Context, id string) ([]users.Device, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []users.Device{
		{ID: "dev_app", Client: "cocoonbox-app", Label: "laptop", CreatedAt: f.used, UsedAt: &f.used},
		{ID: "dev_cli", Client: "cocoonbox-cli", Label: "laptop", CreatedAt: f.used},
	}, nil
}

func (f *fakeUsers) TokenDevice(sum string) string { return f.devices[sum] }

func (f *fakeUsers) RevokeDevice(_ context.Context, id, device string) error {
	if device == "dev_bob" {
		return users.ErrNotFound
	}
	f.revoked = id + "/" + device
	return nil
}

func (f *fakeUsers) RevokeToken(_ context.Context, id string) error {
	if f.err != nil {
		return f.err
	}
	f.tokenRevoked = id
	return nil
}

type noShares struct{}

func (noShares) ShareBySecretSHA256(string) (auth.Share, bool) { return auth.Share{}, false }
