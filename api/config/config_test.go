package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	signingKey = "provision:\n  signing_key_file: edge-signing.key\n"
	gwAdmin    = "  gw_admin:\n    url: http://10.203.0.1:18090\n    token_file: gw-admin-token\n"
	accounts   = "accounts:\n  dsn_file: pg.dsn\n  master_key_file: master.key\n"
	required   = "image: cbdesk:v1\nsandboxd:\n  url: http://127.0.0.1:17780\n  token_file: sandboxd-token\n" + accounts + signingKey + gwAdmin
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, required))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:18200" || cfg.MCPListen != "127.0.0.1:18201" || cfg.AgentPort != 7443 {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.Flavor != (Flavor{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40}) {
		t.Errorf("flavor = %+v", cfg.Flavor)
	}
	if cfg.State.GC.Interval != 0 || cfg.State.GC.DeleteBatch != 1000 {
		t.Errorf("gc = %+v, want off with batches of 1000", cfg.State.GC)
	}
	if cfg.MaxRunningBoxes != 1 {
		t.Errorf("max running boxes = %d, want 1", cfg.MaxRunningBoxes)
	}
	if cfg.Wake != (Wake{MaxSchedules: 3, Floor: Duration(time.Hour), MaxAwakeFor: Duration(time.Hour)}) {
		t.Errorf("wake = %+v, want 3 schedules, a 1h floor and 1h awake at most", cfg.Wake)
	}
	if *cfg.State.ParkAfter != Duration(24*time.Hour) {
		t.Errorf("park after = %s, want 24h", time.Duration(*cfg.State.ParkAfter))
	}
}

func TestLoadParkingOff(t *testing.T) {
	cfg, err := Load(writeConfig(t, required+"state:\n  park_after: 0s\n"))
	if err != nil || *cfg.State.ParkAfter != 0 {
		t.Errorf("Load = %v, %v, want parking off", cfg.State.ParkAfter, err)
	}
}

func TestLoadTheBoxDoor(t *testing.T) {
	cfg, err := Load(writeConfig(t, required+"connect:\n  public_url: https://cb.example/\n  box_listen: 10.203.0.1:18091\n"))
	if err != nil || cfg.Connect.BoxListen != "10.203.0.1:18091" || cfg.Connect.PublicURL != "https://cb.example" {
		t.Errorf("Load = %+v, %v", cfg, err)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{name: "unknown key", body: "listn: x\n", want: "unknown field"},
		{name: "retired users", body: required + "users: []\n", want: "unknown field"},
		{name: "retired scheduled users", body: required + "state:\n  scheduled_users: []\n", want: "unknown field"},
		{name: "no accounts", body: signingKey, want: "accounts.dsn_file and accounts.master_key_file are required"},
		{name: "bad port", body: "agent_port: 70000\n", want: "agent_port"},
		{name: "sandboxd without url", body: "sandboxd:\n  token_file: t\n", want: "sandboxd.url"},
		{name: "no image", body: signingKey, want: "image is required"},
		{name: "retired backend", body: required + "backend: sandboxd\n", want: "unknown field"},
		{name: "retired kubeconfig", body: required + "kubeconfig: kubeconfig\n", want: "unknown field"},
		{name: "retired namespace", body: required + "namespace: cocoonbox\n", want: "unknown field"},
		{name: "retired network", body: required + "network: cb\n", want: "unknown field"},
		{name: "retired probe port", body: required + "probe_port: 7443\n", want: "unknown field"},
		{name: "no signing key", body: "image: x\n", want: "provision.signing_key_file is required"},
		{name: "retired edge token", body: signingKey + "edge_token_file: edge-token\n", want: "unknown field"},
		{name: "retired gw key socket", body: signingKey + "  gw_key_socket: /run/cb-gw-key.sock\n", want: "unknown field"},
		{name: "gw admin without token", body: signingKey + "  gw_admin:\n    url: http://10.203.0.1:18090\n", want: "provision.gw_admin.url and provision.gw_admin.token_file"},
		{name: "no gw admin", body: signingKey, want: "provision.gw_admin.url and provision.gw_admin.token_file"},
		{name: "gc without a bucket", body: required + "state:\n  gc:\n    interval: 24h\n", want: "state.gc.interval"},
		{name: "negative running limit", body: required + "max_running_boxes: -1\n", want: "max_running_boxes must be at least 1"},
		{name: "gc batch over the S3 limit", body: required + "state:\n  gc:\n    delete_batch: 1001\n", want: "state.gc.delete_batch must be in 1..1000"},
		{name: "connect over http", body: required + "connect:\n  public_url: http://cb.example\n", want: "connect.public_url must be an https origin"},
		{name: "connect with a path", body: required + "connect:\n  public_url: https://cb.example/x\n", want: "connect.public_url must be an https origin"},
		{name: "box door on every address", body: required + "connect:\n  public_url: https://cb.example\n  box_listen: 0.0.0.0:18091\n", want: "connect.box_listen must be an ip:port of one address"},
		{name: "box door without connect", body: required + "connect:\n  box_listen: 10.203.0.1:18091\n", want: "connect.box_listen must be an ip:port of one address"},
		{name: "negative wake floor", body: required + "wake:\n  floor: -1h\n", want: "wake.max_schedules and wake.floor must not be negative"},
		{name: "wake awake under a minute", body: required + "wake:\n  max_awake_for: 30s\n", want: "wake.max_awake_for must be at least 1m"},
		{name: "negative park after", body: required + "state:\n  park_after: -1h\n", want: "state.park_after must not be negative"},
		{name: "relative cua meter", body: required + "cua_meter_file: cua-meter.jsonl\n", want: "cua_meter_file must be an absolute path"},
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
	path := writeConfig(t, required)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dir := filepath.Dir(path)
	if cfg.Provision.SigningKeyFile != filepath.Join(dir, "edge-signing.key") || cfg.Sandboxd.TokenFile != filepath.Join(dir, "sandboxd-token") {
		t.Errorf("paths = %q, %q, want under %s", cfg.Provision.SigningKeyFile, cfg.Sandboxd.TokenFile, dir)
	}
	if cfg.Provision.GWAdmin.TokenFile != filepath.Join(dir, "gw-admin-token") {
		t.Errorf("gw admin token file = %q, want under %s", cfg.Provision.GWAdmin.TokenFile, dir)
	}
	if cfg.Accounts.DSNFile != filepath.Join(dir, "pg.dsn") || cfg.Accounts.MasterKeyFile != filepath.Join(dir, "master.key") {
		t.Errorf("accounts = %+v, want under %s", cfg.Accounts, dir)
	}
}

func TestLoadsTheExampleConfig(t *testing.T) {
	cfg, err := Load("../deploy/api.yaml.example")
	if err != nil {
		t.Fatalf("Load example: %v", err)
	}
	if cfg.State.Bucket == "" || cfg.State.GC.Interval != Duration(24*time.Hour) || cfg.Provision.SigningKeyFile == "" || cfg.Accounts.DSNFile == "" || cfg.Image != "cbdesk:v11" {
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
