package users

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDeviceTokensServeBesideTheAPITokenUntilRevoked(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	u, err := svc.Create(t.Context(), NewUser{Name: "qa1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	other, err := svc.Create(t.Context(), NewUser{Name: "qa2"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	personal, err := svc.RegenerateToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	cli, err := svc.IssueDevice(t.Context(), u.ID, "cocoonbox-cli", "laptop")
	if err != nil {
		t.Fatalf("IssueDevice: %v", err)
	}
	app, err := svc.IssueDevice(t.Context(), u.ID, "cocoonbox-app", "laptop")
	if err != nil {
		t.Fatalf("IssueDevice: %v", err)
	}
	if !strings.HasPrefix(cli, TokenPrefix) || len(cli) != len(personal) || cli == app {
		t.Fatalf("device tokens %q and %q, want two distinct tokens shaped like %q", cli, app, personal)
	}
	if _, err := svc.RegenerateToken(t.Context(), u.ID); err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	for _, token := range []string{cli, app} {
		if id, ok := svc.ByTokenSHA256(t.Context(), sum(token)); !ok || id != u.ID {
			t.Errorf("device token after a regenerate resolves to %q, %t, want %s", id, ok, u.ID)
		}
	}
	devices, err := svc.Devices(t.Context(), u.ID)
	if err != nil || len(devices) != 2 || devices[0].Client != "cocoonbox-app" || devices[1].Client != "cocoonbox-cli" || devices[1].Label != "laptop" {
		t.Fatalf("Devices = %+v, %v, want the app's then the CLI's", devices, err)
	}
	if err := svc.RevokeDevice(t.Context(), other.ID, devices[1].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("RevokeDevice of another user's device = %v, want ErrNotFound", err)
	}
	if err := svc.RevokeDevice(t.Context(), u.ID, devices[1].ID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(cli)); ok {
		t.Error("a revoked device token still resolves")
	}
	if id, ok := svc.ByTokenSHA256(t.Context(), sum(app)); !ok || id != u.ID {
		t.Errorf("the other device token resolves to %q, %t after one revoke", id, ok)
	}
	if err := svc.RevokeDevice(t.Context(), u.ID, devices[1].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("RevokeDevice again = %v, want ErrNotFound", err)
	}
	if err := svc.RevokeDeviceSHA256(t.Context(), sum(app)); err != nil {
		t.Fatalf("RevokeDeviceSHA256: %v", err)
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(app)); ok {
		t.Error("a device token revoked by its hash still resolves")
	}
	if err := svc.RevokeDeviceSHA256(t.Context(), sum(personal)); err != nil {
		t.Errorf("RevokeDeviceSHA256 of a token that is no device's = %v, want nil", err)
	}
	if devices, err := svc.Devices(t.Context(), u.ID); err != nil || len(devices) != 0 {
		t.Errorf("Devices after both revokes = %+v, %v", devices, err)
	}
}

func TestDisablingOrDeletingAUserStopsItsDeviceTokens(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	u, err := svc.Create(t.Context(), NewUser{Name: "qa1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	token, err := svc.IssueDevice(t.Context(), u.ID, "cocoonbox-cli", "laptop")
	if err != nil {
		t.Fatalf("IssueDevice: %v", err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
		t.Error("a disabled user's device token resolves")
	}
	if _, err := svc.IssueDevice(t.Context(), u.ID, "cocoonbox-cli", "laptop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("IssueDevice for a disabled user = %v, want ErrNotFound", err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: new(false)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if id, ok := svc.ByTokenSHA256(t.Context(), sum(token)); !ok || id != u.ID {
		t.Errorf("device token after re-enabling resolves to %q, %t", id, ok)
	}
	if err := svc.Delete(t.Context(), u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); ok {
		t.Error("a deleted user's device token resolves")
	}
	var rows int
	if err := query(t, dsn, "SELECT count(*) FROM user_tokens").Scan(&rows); err != nil || rows != 0 {
		t.Errorf("user_tokens rows after Delete = %d, %v", rows, err)
	}
}

func TestADeviceTokenAnotherProcessIssuedWorksAtOnceAndItsRevokeReachesListeners(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	go svc.Listen(t.Context())
	other := openTestService(t, dsn, gw, tenants)
	u, err := other.Create(t.Context(), NewUser{Name: "qa1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	token, err := other.IssueDevice(t.Context(), u.ID, "cocoonbox-cli", "laptop")
	if err != nil {
		t.Fatalf("IssueDevice: %v", err)
	}
	if took, ok := within(time.Second, func() bool { id, ok := svc.ByTokenSHA256(t.Context(), sum(token)); return ok && id == u.ID }); !ok {
		t.Fatalf("a device token another process issued is unknown after %s", took)
	}
	if err := other.RevokeDeviceSHA256(t.Context(), sum(token)); err != nil {
		t.Fatalf("RevokeDeviceSHA256: %v", err)
	}
	if took, ok := within(time.Second, func() bool { _, ok := svc.ByTokenSHA256(t.Context(), sum(token)); return !ok }); !ok {
		t.Errorf("a device token another process revoked still resolves after %s", took)
	}
}

func TestACredentialsLifetimeEndsWithItsRevokeAndWithItsUser(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	stranger, _ := svc.Create(t.Context(), NewUser{Name: "qa2"})
	old, _ := svc.RegenerateToken(t.Context(), u.ID)
	cli, _ := svc.IssueDevice(t.Context(), u.ID, "cocoonbox-cli", "laptop")
	app, _ := svc.IssueDevice(t.Context(), u.ID, "cocoonbox-app", "laptop")
	elsewhere := openTestService(t, dsn, gw, tenants)
	tokenLife, cliLife, appLife, grantLife := svc.Lifetime(u.ID, sum(old)), svc.Lifetime(u.ID, sum(cli)), svc.Lifetime(u.ID, sum(app)), svc.Lifetime(u.ID, "oag_0123")
	cliElsewhere := elsewhere.Lifetime(u.ID, sum(cli))
	for name, life := range map[string]error{"token": tokenLife.Err(), "cli": cliLife.Err(), "app": appLife.Err(), "grant": grantLife.Err(), "cli elsewhere": cliElsewhere.Err()} {
		if life != nil {
			t.Fatalf("the %s lifetime ended before any revoke: %v", name, life)
		}
	}
	if svc.Lifetime(stranger.ID, sum(cli)).Err() == nil || svc.Lifetime(u.ID, sum("cb_unknown")).Err() == nil {
		t.Error("a lifetime for another user's token or an unknown token is live")
	}
	if _, err := svc.RegenerateToken(t.Context(), u.ID); err != nil || tokenLife.Err() == nil {
		t.Errorf("RegenerateToken = %v, the replaced token's lifetime %v", err, tokenLife.Err())
	}
	devices, _ := svc.Devices(t.Context(), u.ID)
	if err := svc.RevokeDevice(t.Context(), u.ID, devices[1].ID); err != nil || cliLife.Err() == nil || appLife.Err() != nil {
		t.Errorf("RevokeDevice = %v, the revoked device's lifetime %v, the other's %v", err, cliLife.Err(), appLife.Err())
	}
	if err := elsewhere.Load(t.Context()); err != nil || cliElsewhere.Err() == nil {
		t.Errorf("another api reloading the revoke = %v, its lifetime %v", err, cliElsewhere.Err())
	}
	svc.EndCredential("oag_0123")
	if grantLife.Err() == nil || svc.Lifetime(u.ID, "oag_0123").Err() != nil {
		t.Errorf("after EndCredential the grant's lifetime %v, a new one %v", grantLife.Err(), svc.Lifetime(u.ID, "oag_0123").Err())
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil || appLife.Err() == nil {
		t.Errorf("disable = %v, a device's lifetime %v", err, appLife.Err())
	}
}

func TestADeviceRecordsItsUseOncePerMinute(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	u, err := svc.Create(t.Context(), NewUser{Name: "qa1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	token, err := svc.IssueDevice(t.Context(), u.ID, "cocoonbox-cli", "laptop")
	if err != nil {
		t.Fatalf("IssueDevice: %v", err)
	}
	if devices, err := svc.Devices(t.Context(), u.ID); err != nil || len(devices) != 1 || devices[0].UsedAt != nil {
		t.Fatalf("Devices before any use = %+v, %v", devices, err)
	}
	svc.ByTokenSHA256(t.Context(), sum(token))
	first, err := svc.Devices(t.Context(), u.ID)
	if err != nil || len(first) != 1 || first[0].UsedAt == nil {
		t.Fatalf("Devices after a use = %+v, %v", first, err)
	}
	svc.ByTokenSHA256(t.Context(), sum(token))
	second, err := svc.Devices(t.Context(), u.ID)
	if err != nil || len(second) != 1 || second[0].UsedAt == nil || !second[0].UsedAt.Equal(*first[0].UsedAt) {
		t.Errorf("Devices after a second use within a minute = %+v, %v, want used_at %v", second, err, first[0].UsedAt)
	}
}
