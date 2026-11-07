package users

import (
	"testing"
)

func TestPushesAreRecordedOncePerMachineKindAndNodeAndDroppedByNode(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	u, err := svc.Create(t.Context(), NewUser{Name: "alice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for range 2 {
		if err := svc.RecordPushes(t.Context(), "m1", u.ID, "n1", []string{PushModel, PushDoor}); err != nil {
			t.Fatalf("RecordPushes: %v", err)
		}
	}
	if err := svc.RecordPushes(t.Context(), "m1", u.ID, "n2", []string{PushVault}); err != nil {
		t.Fatalf("RecordPushes n2: %v", err)
	}
	pushes, err := svc.PushesOn(t.Context(), "n1")
	if err != nil || len(pushes) != 2 || pushes[0].Kind != PushDoor || pushes[1].Kind != PushModel || pushes[0].Owner != u.ID || pushes[0].At.IsZero() {
		t.Fatalf("PushesOn n1 = %+v, %v; want door and model once each", pushes, err)
	}
	if err := svc.DropPushesOn(t.Context(), "n1"); err != nil {
		t.Fatalf("DropPushesOn: %v", err)
	}
	if pushes, _ := svc.PushesOn(t.Context(), "n1"); len(pushes) != 0 {
		t.Errorf("PushesOn n1 after the drop = %+v", pushes)
	}
	if pushes, _ := svc.PushesOn(t.Context(), "n2"); len(pushes) != 1 {
		t.Errorf("PushesOn n2 = %+v, want the other node's push kept", pushes)
	}
}
