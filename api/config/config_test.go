package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	validHash  = "2bd806c97f0e00af1a1fc3328fa763a9269723c8db8fac4f93af71db186d6e90"
	signingKey = "provision:\n  signing_key_file: edge-signing.key\n"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, signingKey+"users:\n  - name: alice\n    token_sha256: "+validHash+"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:18200" || cfg.MCPListen != "127.0.0.1:18201" || cfg.Namespace != "cocoonbox" || cfg.Image != "cocoonbox-desktop-v1" ||
		cfg.Network != "cb" || cfg.ProbePort != 22 || cfg.AgentPort != 7443 {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.Backend != BackendCocoonSet {
		t.Errorf("backend = %q", cfg.Backend)
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
		{name: "unknown backend", body: "backend: nomad\n", want: "backend must be"},
		{name: "sandboxd without url", body: "backend: sandboxd\nsandboxd:\n  token_file: t\n", want: "sandboxd.url"},
		{name: "no signing key", body: "users: []\n", want: "provision.signing_key_file is required"},
		{name: "retired edge token", body: signingKey + "edge_token_file: edge-token\n", want: "unknown field"},
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
	path := writeConfig(t, signingKey+"kubeconfig: kubeconfig\nbackend: sandboxd\nsandboxd:\n  url: http://127.0.0.1:17800\n  token_file: sandboxd-token\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dir := filepath.Dir(path)
	if cfg.Kubeconfig != filepath.Join(dir, "kubeconfig") || cfg.Provision.SigningKeyFile != filepath.Join(dir, "edge-signing.key") || cfg.Sandboxd.TokenFile != filepath.Join(dir, "sandboxd-token") {
		t.Errorf("paths = %q, %q, %q, want under %s", cfg.Kubeconfig, cfg.Provision.SigningKeyFile, cfg.Sandboxd.TokenFile, dir)
	}
}

func TestLoadsTheExampleConfig(t *testing.T) {
	cfg, err := Load("../deploy/api.yaml.example")
	if err != nil {
		t.Fatalf("Load example: %v", err)
	}
	if cfg.State.Bucket == "" || cfg.Provision.SigningKeyFile == "" || len(cfg.Users) != 1 || cfg.Image != "cocoonbox-desktop-v7" {
		t.Errorf("example config = %+v", cfg)
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
