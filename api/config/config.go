// Package config loads the cocoonbox-api configuration file.
package config

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

const (
	defaultListen        = "127.0.0.1:18200"
	defaultMCPListen     = "127.0.0.1:18201"
	defaultNamespace     = "cocoonbox"
	defaultImage         = "cocoonbox-desktop-v1"
	defaultNetwork       = "cb"
	defaultProbePort     = 22
	defaultAgentPort     = 7443
	defaultEdgeTokenFile = "/etc/cocoonbox/edge-token" //nolint:gosec // a path, not a credential

	defaultStateInterval = 15 * time.Minute
	defaultPresignTTL    = time.Hour
	defaultKeyringDir    = "/var/lib/cocoonbox-api/keys"
	defaultRegion        = "us-east-1"

	defaultVCPUs     = 4
	defaultMemoryMiB = 8192
	defaultDiskGiB   = 40
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Flavor is the default box size.
type Flavor struct {
	VCPUs     uint32 `json:"vcpus"`
	MemoryMiB uint32 `json:"memory_mib"`
	DiskGiB   uint32 `json:"disk_gib"`
}

// User maps a user name to the sha256 of its bearer token.
type User struct {
	Name        string `json:"name"`
	TokenSHA256 string `json:"token_sha256"`
}

// Duration is a time.Duration written as a Go duration string ("15m").
type Duration time.Duration

func (d *Duration) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return fmt.Errorf("duration must be a string like \"15m\": %w", err)
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// State configures the object store for user state; an empty Bucket disables the state layer.
type State struct {
	Endpoint string `json:"endpoint"`
	// PublicEndpoint is the host:port box agents use; empty uses Endpoint.
	PublicEndpoint string `json:"public_endpoint"`
	Secure         bool   `json:"secure"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	// CredentialsFile holds STORE_ACCESS_KEY and STORE_SECRET_KEY lines.
	CredentialsFile string   `json:"credentials_file"`
	KeyringDir      string   `json:"keyring_dir"`
	Interval        Duration `json:"interval"`
	// ScheduledUsers limits scheduled saves to these users; empty schedules everyone.
	ScheduledUsers []string `json:"scheduled_users"`
	PresignTTL     Duration `json:"presign_ttl"`
}

// Config is the on-disk configuration of cocoonbox-api.
type Config struct {
	Listen        string `json:"listen"`
	MCPListen     string `json:"mcp_listen"`
	Kubeconfig    string `json:"kubeconfig"`
	Namespace     string `json:"namespace"`
	Image         string `json:"image"`
	Network       string `json:"network"`
	ProbePort     int32  `json:"probe_port"`
	AgentPort     int    `json:"agent_port"`
	EdgeTokenFile string `json:"edge_token_file"`
	Flavor        Flavor `json:"flavor"`
	State         State  `json:"state"`
	Users         []User `json:"users"`
}

// Load reads, defaults and validates the file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	cfg.resolvePaths(filepath.Dir(path))
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

// StoreCredentials reads the access and secret key from State.CredentialsFile.
func (c *Config) StoreCredentials() (string, string, error) {
	raw, err := os.ReadFile(c.State.CredentialsFile)
	if err != nil {
		return "", "", fmt.Errorf("read store credentials: %w", err)
	}
	values := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[k] = v
		}
	}
	access, secret := values["STORE_ACCESS_KEY"], values["STORE_SECRET_KEY"]
	if access == "" || secret == "" {
		return "", "", fmt.Errorf("%s must set STORE_ACCESS_KEY and STORE_SECRET_KEY", c.State.CredentialsFile)
	}
	return access, secret, nil
}

// EdgeToken reads the shared secret the edge presents to box agents.
func (c *Config) EdgeToken() (string, error) {
	raw, err := os.ReadFile(c.EdgeTokenFile)
	if err != nil {
		return "", fmt.Errorf("read edge token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("edge token file %s is empty", c.EdgeTokenFile)
	}
	return token, nil
}

func (c *Config) applyDefaults() {
	c.Listen = cmp.Or(c.Listen, defaultListen)
	c.MCPListen = cmp.Or(c.MCPListen, defaultMCPListen)
	c.Namespace = cmp.Or(c.Namespace, defaultNamespace)
	c.Image = cmp.Or(c.Image, defaultImage)
	c.Network = cmp.Or(c.Network, defaultNetwork)
	c.ProbePort = cmp.Or(c.ProbePort, defaultProbePort)
	c.AgentPort = cmp.Or(c.AgentPort, defaultAgentPort)
	c.EdgeTokenFile = cmp.Or(c.EdgeTokenFile, defaultEdgeTokenFile)
	c.Flavor.VCPUs = cmp.Or(c.Flavor.VCPUs, defaultVCPUs)
	c.Flavor.MemoryMiB = cmp.Or(c.Flavor.MemoryMiB, defaultMemoryMiB)
	c.Flavor.DiskGiB = cmp.Or(c.Flavor.DiskGiB, defaultDiskGiB)
	c.State.Region = cmp.Or(c.State.Region, defaultRegion)
	c.State.KeyringDir = cmp.Or(c.State.KeyringDir, defaultKeyringDir)
	c.State.Interval = cmp.Or(c.State.Interval, Duration(defaultStateInterval))
	c.State.PresignTTL = cmp.Or(c.State.PresignTTL, Duration(defaultPresignTTL))
}

// resolvePaths anchors relative file paths at dir, which lets systemd LoadCredential= hand over all files in one directory.
func (c *Config) resolvePaths(dir string) {
	for _, p := range []*string{&c.Kubeconfig, &c.EdgeTokenFile, &c.State.CredentialsFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	names := map[string]bool{}
	hashes := map[string]bool{}
	for i, u := range c.Users {
		if u.Name == "" {
			errs = append(errs, fmt.Errorf("users[%d]: name must not be empty", i))
		}
		if !sha256Hex.MatchString(u.TokenSHA256) {
			errs = append(errs, fmt.Errorf("users[%d]: token_sha256 must be 64 lowercase hex digits", i))
		}
		if names[u.Name] {
			errs = append(errs, fmt.Errorf("users[%d]: duplicate name %q", i, u.Name))
		}
		if hashes[u.TokenSHA256] {
			errs = append(errs, fmt.Errorf("users[%d]: duplicate token_sha256", i))
		}
		names[u.Name], hashes[u.TokenSHA256] = true, true
	}
	if c.ProbePort < 0 || c.ProbePort > 65535 {
		errs = append(errs, fmt.Errorf("probe_port must be in 0..65535, got %d", c.ProbePort))
	}
	if c.State.Bucket != "" && (c.State.Endpoint == "" || c.State.CredentialsFile == "") {
		errs = append(errs, errors.New("state.endpoint and state.credentials_file are required with state.bucket"))
	}
	if c.AgentPort < 1 || c.AgentPort > 65535 {
		errs = append(errs, fmt.Errorf("agent_port must be in 1..65535, got %d", c.AgentPort))
	}
	return errors.Join(errs...)
}
