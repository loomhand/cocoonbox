package oauth

import "testing"

func TestAgentScope(t *testing.T) {
	tests := []struct {
		scope       string
		connect, ok bool
	}{
		{"", false, true},
		{"boxes", false, true},
		{"boxes connect", true, true},
		{"connect boxes", true, true},
		{" boxes  boxes ", false, true},
		{"connect", false, false},
		{"boxes keys", false, false},
		{"boxes connect account", false, false},
		{"account", false, false},
	}
	for _, tt := range tests {
		if connect, ok := agentScope(tt.scope); ok != tt.ok || (ok && connect != tt.connect) {
			t.Errorf("agentScope(%q) = %t, %t, want %t, %t", tt.scope, connect, ok, tt.connect, tt.ok)
		}
	}
	if grantedScope(false) != "boxes" || grantedScope(true) != "boxes connect" {
		t.Errorf("granted scopes = %q, %q", grantedScope(false), grantedScope(true))
	}
}
