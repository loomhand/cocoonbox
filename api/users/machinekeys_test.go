package users

import "testing"

func TestMachineKeysAreIssuedPerIncarnationRevokedAndGoWithTheUser(t *testing.T) {
	svc, gw, _, _ := newTestService(t)
	u, err := svc.Create(t.Context(), NewUser{Name: "alice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	key, first, err := svc.IssueMachineKey(t.Context(), u.ID, "m1", "i1")
	if err != nil || key == "" || !gw.Machines[first] {
		t.Fatalf("IssueMachineKey = %q, %q, %v; want an enabled key", key, first, err)
	}
	if _, err := svc.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	_, second, err := svc.IssueMachineKey(t.Context(), u.ID, "m1", "i2")
	if err != nil || gw.Machines[second] {
		t.Fatalf("IssueMachineKey of a disabled user = %q, %v; want an expired key", second, err)
	}
	if _, _, err := svc.IssueMachineKey(t.Context(), u.ID, "m2", "j1"); err != nil {
		t.Fatalf("IssueMachineKey m2: %v", err)
	}

	keys, err := svc.MachineKeys(t.Context(), "m1")
	if err != nil || len(keys) != 2 || keys[0].ID != second || keys[0].Incarnation != "i2" || keys[1].ID != first {
		t.Fatalf("MachineKeys m1 = %+v, %v; want the newest first", keys, err)
	}
	if err := svc.InstallMachineKey(t.Context(), "m1", second); err != nil {
		t.Fatalf("InstallMachineKey: %v", err)
	}
	if keys, _ := svc.MachineKeys(t.Context(), "m1"); !keys[0].Installed || keys[1].Installed {
		t.Errorf("MachineKeys m1 = %+v, want only the second installed", keys)
	}
	if err := svc.RevokeMachineKey(t.Context(), first); err != nil {
		t.Fatalf("RevokeMachineKey: %v", err)
	}
	if _, ok := gw.Machines[first]; ok {
		t.Error("the revoked key stays at gw")
	}
	if all, _ := svc.MachineKeys(t.Context(), ""); len(all) != 2 {
		t.Errorf("MachineKeys = %+v, want two left", all)
	}
	if err := svc.Delete(t.Context(), u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if all, _ := svc.MachineKeys(t.Context(), ""); len(all) != 0 {
		t.Errorf("MachineKeys after the user went = %+v, want none", all)
	}
}
