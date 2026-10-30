package webhook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestAFileBearerFollowsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	bearer := FileBearer(path)
	for _, want := range []string{"first", "rotated"} {
		if err := os.WriteFile(path, []byte(want+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := bearer.Token(t.Context()); err != nil || got != want {
			t.Errorf("Token = %q, %v, want %q", got, err, want)
		}
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bearer.Token(t.Context()); err == nil {
		t.Error("an empty token file gave a token")
	}
}

func TestAPluginGetsItsArgumentsVerbatim(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	body := `printf '%s\n' "$1" > ` + seen + `
echo '{"token":"tok"}'`
	bearer := ExecBearer([]string{plugin(t, dir, body), "a b;$(touch pwned)"}, roomy)
	if got, err := bearer.Token(t.Context()); err != nil || got != "tok" {
		t.Fatalf("Token = %q, %v", got, err)
	}
	if raw, err := os.ReadFile(seen); err != nil || string(raw) != "a b;$(touch pwned)\n" {
		t.Errorf("the plugin saw %q, %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Error("an argument ran as a shell command")
	}
}

func TestAPluginTokenIsKeptUntilItRunsOutOrIsRejected(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	for name, tc := range map[string]struct {
		expires string
		want    int
	}{
		"an hour":       {`"expiresAt":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"`, 1},
		"no expiry":     {`"expiresAt":null`, 1},
		"half a minute": {`"expiresAt":"` + time.Now().Add(30*time.Second).UTC().Format(time.RFC3339) + `"`, 3},
	} {
		_ = os.Remove(runs)
		body := `echo run >> "$1"; printf '{"token":"tok-%s",` + tc.expires + `}' "$(wc -l < "$1" | tr -d ' ')"`
		bearer := ExecBearer([]string{plugin(t, dir, body), runs}, roomy)
		for range 3 {
			if _, err := bearer.Token(t.Context()); err != nil {
				t.Fatalf("%s: Token: %v", name, err)
			}
		}
		if n := lines(t, runs); n != tc.want {
			t.Errorf("%s: the plugin ran %d times, want %d", name, n, tc.want)
		}
		token, _ := bearer.Token(t.Context())
		bearer.Rejected("someone else's")
		if again, _ := bearer.Token(t.Context()); again != token && tc.want == 1 {
			t.Errorf("%s: rejecting another token changed ours to %q", name, again)
		}
		bearer.Rejected(token)
		if again, err := bearer.Token(t.Context()); err != nil || again == token {
			t.Errorf("%s: after a rejection Token = %q, %v, want a new one", name, again, err)
		}
	}
}

func TestAPluginThatFailsGivesNoToken(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		body    string
		timeout time.Duration
	}{
		"exits":    {"echo secret-on-stderr >&2; exit 3", roomy},
		"garbles":  {"echo not json", roomy},
		"no token": {`echo '{"expiresAt":"2099-01-01T00:00:00Z"}'`, roomy},
		"hangs":    {"exec sleep 30", 200 * time.Millisecond},
	} {
		bearer := ExecBearer([]string{plugin(t, filepath.Join(dir, strings.ReplaceAll(name, " ", "-")), tc.body)}, tc.timeout)
		start := time.Now()
		got, err := bearer.Token(t.Context())
		if err == nil || got != "" || strings.Contains(err.Error(), "secret") || time.Since(start) > 5*time.Second {
			t.Errorf("%s: Token = %q, %v after %s", name, got, err, time.Since(start))
		}
	}
}

func TestConcurrentCallsShareOneRunOfThePlugin(t *testing.T) {
	dir := t.TempDir()
	runs, gate := filepath.Join(dir, "runs"), filepath.Join(dir, "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	// the plugin fails once the test opens the gate, so no token is kept and every call needs this run's answer
	bearer := ExecBearer([]string{plugin(t, dir, `echo run >> "$1"; read -r _ < "$2"; exit 1`), runs, gate}, roomy)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			_, err := bearer.Token(t.Context())
			errs <- err
		})
	}
	for deadline := time.Now().Add(roomy); !exists(runs); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the plugin never started")
		}
	}
	open, err := os.OpenFile(gate, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = open.WriteString("go\n")
	_ = open.Close()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Error("a failing plugin gave a token")
		}
	}
	if n := lines(t, runs); n != 1 {
		t.Errorf("the plugin ran %d times for 4 calls at once, want once", n)
	}
}

func TestACallStopsWaitingForThePluginWhenItsContextEnds(t *testing.T) {
	dir := t.TempDir()
	bearer := ExecBearer([]string{plugin(t, dir, "exec sleep 30")}, roomy)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := bearer.Token(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 500*time.Millisecond {
		t.Errorf("Token with a 50ms context = %v after %s", err, time.Since(start))
	}
}

// roomy is a plugin timeout no test means to reach, however slowly a loaded host starts the plugin.
const roomy = 10 * time.Second

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// plugin writes a credential plugin in dir that runs body under sh.
func plugin(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plugin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func lines(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
}
