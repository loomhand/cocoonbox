package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validHash = "2bd806c97f0e00af1a1fc3328fa763a9269723c8db8fac4f93af71db186d6e90"

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "users:\n  - name: alice\n    token_sha256: "+validHash+"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:18200" || cfg.MCPListen != "127.0.0.1:18201" || cfg.Namespace != "cocoonbox" || cfg.Image != "cocoonbox-desktop-v1" ||
		cfg.Network != "cb" || cfg.ProbePort != 22 || cfg.AgentPort != 7443 {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.Flavor != (Flavor{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40}) {
		t.Errorf("flavor = %+v", cfg.Flavor)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{name: "unknown key", body: "listn: x\n", want: "unknown field"},
		{name: "bad hash", body: "users:\n  - name: alice\n    token_sha256: abc\n", want: "64 lowercase hex"},
		{name: "duplicate user", body: "users:\n  - name: a\n    token_sha256: " + validHash + "\n  - name: a\n    token_sha256: " + strings.Repeat("0", 64) + "\n", want: "duplicate name"},
		{name: "bad port", body: "agent_port: 70000\n", want: "agent_port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, tt.body)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load: %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadResolvesRelativePaths(t *testing.T) {
	path := writeConfig(t, "kubeconfig: kubeconfig\nedge_token_file: edge-token\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dir := filepath.Dir(path)
	if cfg.Kubeconfig != filepath.Join(dir, "kubeconfig") || cfg.EdgeTokenFile != filepath.Join(dir, "edge-token") {
		t.Errorf("paths = %q, %q, want under %s", cfg.Kubeconfig, cfg.EdgeTokenFile, dir)
	}
}

func TestEdgeToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge-token")
	if err := os.WriteFile(path, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	cfg := &Config{EdgeTokenFile: path}
	if got, err := cfg.EdgeToken(); err != nil || got != "s3cret" {
		t.Errorf("EdgeToken = %q, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := cfg.EdgeToken(); err == nil {
		t.Error("EdgeToken accepted an empty file")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return path
}
