package webhook

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// refreshBefore runs the plugin again this long before its token expires, so no call goes out with a token about to lapse.
const refreshBefore = time.Minute

// Bearer gives the api's own token for the endpoint, and drops one the endpoint answered 401 so the next call gets a new one.
type Bearer interface {
	Token(ctx context.Context) (string, error)
	Rejected(token string)
}

// FileBearer reads the api's token from path on every call, so whatever rotates the file is picked up.
func FileBearer(path string) Bearer {
	return fileBearer(path)
}

// ExecBearer runs the credential plugin argv, without a shell, for {"token", "expiresAt"} on stdout, in the way of a Kubernetes
// exec credential plugin; it keeps the token until a minute before expiresAt, or without one until the endpoint rejects it.
// Calls that need it while it runs share that run and stop waiting when their context ends; neither its output nor its
// stderr reaches a log.
func ExecBearer(argv []string, timeout time.Duration) Bearer {
	return &execBearer{argv: argv, timeout: timeout}
}

type fileBearer string

func (f fileBearer) Token(context.Context) (string, error) {
	raw, err := os.ReadFile(string(f))
	if err != nil {
		return "", fmt.Errorf("read the identity webhook token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("identity webhook token file %s is empty", string(f))
	}
	return token, nil
}

func (fileBearer) Rejected(string) {}

type execBearer struct {
	argv    []string
	timeout time.Duration

	mu      sync.Mutex
	token   string
	expires *time.Time
	running *flight
}

func (e *execBearer) Token(ctx context.Context) (string, error) {
	e.mu.Lock()
	if e.token != "" && (e.expires == nil || time.Now().Before(e.expires.Add(-refreshBefore))) {
		defer e.mu.Unlock()
		return e.token, nil
	}
	f := e.running
	if f == nil {
		f = &flight{done: make(chan struct{})}
		e.running = f
		go e.fly(context.WithoutCancel(ctx), f)
	}
	e.mu.Unlock()
	select {
	case <-f.done:
		return f.token, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (e *execBearer) Rejected(token string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.token == token {
		e.token = ""
	}
}

// fly runs the plugin for f, bounded by the timeout and not by any one caller, and keeps a token it gets.
func (e *execBearer) fly(ctx context.Context, f *flight) {
	token, expires, err := e.run(ctx)
	e.mu.Lock()
	if err == nil {
		e.token, e.expires = token, expires
	}
	e.running = nil
	e.mu.Unlock()
	f.token, f.err = token, err
	close(f.done)
}

func (e *execBearer) run(ctx context.Context) (string, *time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, e.argv[0], e.argv[1:]...) //nolint:gosec // the operator's credential plugin, run without a shell
	cmd.Stdout = &out
	// a child that keeps stdout open does not hold the call past the timeout
	cmd.WaitDelay = e.timeout
	if err := cmd.Run(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", nil, fmt.Errorf("the credential plugin exited with %d", exit.ExitCode())
		}
		return "", nil, fmt.Errorf("run the credential plugin: %w", err)
	}
	var cred struct {
		Token     string     `json:"token"`
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(out.Bytes(), &cred); err != nil || cred.Token == "" {
		return "", nil, errors.New(`the credential plugin printed no {"token", "expiresAt"}`)
	}
	return cred.Token, cred.ExpiresAt, nil
}

// flight is one run of the plugin that every call arriving meanwhile waits on.
type flight struct {
	done  chan struct{}
	token string
	err   error
}
