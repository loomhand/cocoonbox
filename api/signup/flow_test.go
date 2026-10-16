package signup

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)

func TestUsernamesComeFromTheLoginAndFitTheUserNameGrammar(t *testing.T) {
	tests := []struct {
		login string
		first string
	}{
		{"Bob", "bob"},
		{"octo.Cat_1", "octo-cat_1"},
		{"1234", "gh-1234"},
		{"a", "gh-a"},
		{"-x", "gh-x"},
		{strings.Repeat("long", 20), strings.Repeat("long", 7) + "l"},
	}
	for _, tt := range tests {
		t.Run(tt.login, func(t *testing.T) {
			names := slices.Collect(usernames(tt.login))
			if names[0] != tt.first || names[1] != tt.first+"-2" || len(names) != maxNameTries {
				t.Errorf("usernames(%q) = %v…, want %q, %q-2, …", tt.login, names[:2], tt.first, tt.first)
			}
			for _, n := range names {
				if !namePattern.MatchString(n) {
					t.Errorf("username %q breaks the user name grammar", n)
				}
			}
		})
	}
}

func TestOnlyAConsolePathIsAReturnPath(t *testing.T) {
	for path, want := range map[string]string{
		"/cb/oauth/authorize?x=1":  "/cb/oauth/authorize?x=1",
		"":                         "/cb/",
		"https://evil.example/cb/": "/cb/",
		"//evil.example/cb/":       "/cb/",
		"/cb/\\evil":               "/cb/",
		"/other":                   "/cb/",
	} {
		if got := safeReturn(path); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", path, got, want)
		}
	}
}
