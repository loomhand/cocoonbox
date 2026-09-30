// Package gwkeys talks to the host helper that issues per-box gw keys.
package gwkeys

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"
)

const dialTimeout = 5 * time.Second

var gwKey = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// Client sends one line per connection to the helper's unix socket.
type Client struct {
	socket string
}

// New talks to the helper at socket.
func New(socket string) *Client {
	return &Client{socket: socket}
}

// Add returns box's gw key, creating it on first use.
func (c *Client) Add(ctx context.Context, box string) (string, error) {
	reply, err := c.call(ctx, "add "+box)
	if err != nil {
		return "", err
	}
	if !gwKey.MatchString(reply) {
		return "", fmt.Errorf("gw key helper returned a malformed key for %s", box)
	}
	return reply, nil
}

// Revoke removes box's gw key; revoking an absent key succeeds.
func (c *Client) Revoke(ctx context.Context, box string) error {
	_, err := c.call(ctx, "revoke "+box)
	return err
}

func (c *Client) call(ctx context.Context, line string) (string, error) {
	var d net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := d.DialContext(dialCtx, "unix", c.socket)
	if err != nil {
		return "", fmt.Errorf("dial gw key helper: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	}
	if _, err = io.WriteString(conn, line+"\n"); err != nil {
		return "", fmt.Errorf("write to gw key helper: %w", err)
	}
	raw, err := io.ReadAll(bufio.NewReader(conn))
	if err != nil {
		return "", fmt.Errorf("read from gw key helper: %w", err)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	verb, rest, _ := strings.Cut(first, " ")
	switch verb {
	case "ok":
		return rest, nil
	case "error":
		return "", fmt.Errorf("gw key helper: %s", rest)
	default:
		return "", fmt.Errorf("gw key helper sent %q", first)
	}
}
