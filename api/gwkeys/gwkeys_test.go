package gwkeys

import (
	"bufio"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddAndRevoke(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "gw.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	lines := make(chan string, 8)
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			line = strings.TrimSpace(line)
			lines <- line
			switch {
			case strings.HasPrefix(line, "add cb-"):
				_, _ = conn.Write([]byte("ok " + strings.Repeat("k3y", 16) + "\n"))
			case strings.HasPrefix(line, "revoke "):
				_, _ = conn.Write([]byte("ok\n"))
			default:
				_, _ = conn.Write([]byte("error invalid box id\n"))
			}
			_ = conn.Close()
		}
	}()
	c := New(sock)
	key, err := c.Add(t.Context(), "cb-ef260e9aa3c6")
	if err != nil || len(key) != 48 {
		t.Fatalf("Add = %q, %v", key, err)
	}
	if err := c.Revoke(t.Context(), "cb-ef260e9aa3c6-rabc"); err != nil {
		t.Errorf("Revoke: %v", err)
	}
	if _, err := c.Add(t.Context(), "nope"); err == nil || !strings.Contains(err.Error(), "invalid box id") {
		t.Errorf("Add of a bad id: %v", err)
	}
	got := []string{<-lines, <-lines, <-lines}
	if strings.Join(got, ";") != "add cb-ef260e9aa3c6;revoke cb-ef260e9aa3c6-rabc;add nope" {
		t.Errorf("helper saw %q", got)
	}
}
