package users

import (
	"encoding/json"
	"testing"
)

func TestToolListFallsBackToTheNewestOfItsFamily(t *testing.T) {
	svc, _, _, dsn := newTestService(t)
	if l, err := svc.ToolList(t.Context(), "cbcore:v1", "cbcore"); err != nil || l.Image != "" {
		t.Fatalf("empty store = %+v, %v", l, err)
	}
	for _, put := range []struct {
		family string
		list   ToolList
	}{
		{"cbcore", ToolList{Image: "cbcore:v1", Tools: json.RawMessage(`[{"name":"shell"}]`), Instructions: "v1"}},
		{"cbcore", ToolList{Image: "cbcore:v2", Tools: json.RawMessage(`[{"name":"shell"},{"name":"gui_plan"}]`), Instructions: "v2"}},
		{"cbdroid", ToolList{Image: "cbdroid:v1", Tools: json.RawMessage(`[]`)}},
	} {
		if err := svc.PutToolList(t.Context(), put.family, put.list); err != nil {
			t.Fatalf("PutToolList %s: %v", put.list.Image, err)
		}
	}
	reopened := openTestService(t, dsn, &fakeGW{keys: map[string]string{}, caps: map[string]int64{}, disabled: map[string]bool{}}, &fakeTenants{tenants: map[string]tenant{}})
	for _, tt := range []struct{ image, family, want, tools string }{
		{"cbcore:v1", "cbcore", "cbcore:v1", `[{"name":"shell"}]`},
		{"cbcore:v3", "cbcore", "cbcore:v2", `[{"name":"shell"},{"name":"gui_plan"}]`},
		{"other:v1", "other", "", ""},
	} {
		l, err := reopened.ToolList(t.Context(), tt.image, tt.family)
		if err != nil || l.Image != tt.want || string(l.Tools) != tt.tools {
			t.Errorf("ToolList(%s) = %+v, %v; want %s", tt.image, l, err, tt.want)
		}
	}
}
