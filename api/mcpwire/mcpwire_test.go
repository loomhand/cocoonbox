package mcpwire

import "testing"

func TestNegotiateKeepsASupportedVersionAndElseAnswersLatest(t *testing.T) {
	for requested, want := range map[string]string{"2025-11-25": "2025-11-25", "2025-06-18": "2025-06-18", "2024-11-05": Latest, "": Latest} {
		if got := Negotiate(requested); got != want {
			t.Errorf("Negotiate(%q) = %q, want %q", requested, got, want)
		}
	}
}

func TestEventAndCodeOf(t *testing.T) {
	if got := string(Event([]byte(`{"a":1}`))); got != "event: message\ndata: {\"a\":1}\n\n" {
		t.Errorf("Event = %q", got)
	}
	if CodeOf(nil) != 0 || CodeOf(&Error{Code: CodeInternal}) != CodeInternal {
		t.Error("CodeOf")
	}
}
