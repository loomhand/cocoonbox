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
	defaultListen    = "127.0.0.1:18200"
	defaultMCPListen = "127.0.0.1:18201"
	defaultAgentPort = 7443

	defaultStateInterval = 15 * time.Minute
	defaultPresignTTL    = 10 * time.Minute
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
	// ScheduledUsers lists the users whose boxes get scheduled saves; empty schedules nobody.
	ScheduledUsers []string `json:"scheduled_users"`
	PresignTTL     Duration `json:"presign_ttl"`
}

// Provision names the edge signing key and gw's admin API for per-box gw keys.
type Provision struct {
	SigningKeyFile string  `json:"signing_key_file"`
	GWAdmin        GWAdmin `json:"gw_admin"`
}

// GWAdmin names gw's admin API and the file holding its cocoonbox-boxes tenant admin token; an empty URL disables gw keys.
type GWAdmin struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
}

// Sandboxd names the sandboxd instance that runs the boxes and the file holding its root api token.
type Sandboxd struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
}

// Config is the on-disk configuration of cocoonbox-api.
type Config struct {
	Sandboxd     Sandboxd `json:"sandboxd"`
	Listen       string   `json:"listen"`
	MCPListen    string   `json:"mcp_listen"`
	Image        string   `json:"image"`
	RebaseImages []string `json:"rebase_images"`
	AgentPort    int      `json:"agent_port"`
	// Provision configures every box's identity file and per-box gw key.
	Provision Provision `json:"provision"`
	Flavor    Flavor    `json:"flavor"`
	Flavors   []Flavor  `json:"flavors"`
	State     State     `json:"state"`
	Users     []User    `json:"users"`
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

// SandboxdToken reads the root api token of the sandboxd instance.
func (c *Config) SandboxdToken() (string, error) {
	return readToken(c.Sandboxd.TokenFile, "sandboxd token")
}

// GWAdminToken reads the tenant admin token of gw's admin API.
func (c *Config) GWAdminToken() (string, error) {
	return readToken(c.Provision.GWAdmin.TokenFile, "gw admin token")
}

func (c *Config) applyDefaults() {
	c.Listen = cmp.Or(c.Listen, defaultListen)
	c.MCPListen = cmp.Or(c.MCPListen, defaultMCPListen)
	c.AgentPort = cmp.Or(c.AgentPort, defaultAgentPort)
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
	for _, p := range []*string{&c.State.CredentialsFile, &c.Provision.SigningKeyFile, &c.Provision.GWAdmin.TokenFile, &c.Sandboxd.TokenFile} {
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
	if c.Provision.SigningKeyFile == "" {
		errs = append(errs, errors.New("provision.signing_key_file is required: agents admit only edge-signed credentials"))
	}
	if (c.Provision.GWAdmin.URL == "") != (c.Provision.GWAdmin.TokenFile == "") {
		errs = append(errs, errors.New("provision.gw_admin.url and provision.gw_admin.token_file go together"))
	}
	if c.State.Bucket != "" && (c.State.Endpoint == "" || c.State.CredentialsFile == "") {
		errs = append(errs, errors.New("state.endpoint and state.credentials_file are required with state.bucket"))
	}
	if c.Sandboxd.URL == "" || c.Sandboxd.TokenFile == "" {
		errs = append(errs, errors.New("sandboxd.url and sandboxd.token_file are required"))
	}
	if c.Image == "" {
		errs = append(errs, errors.New("image is required: it names the default sandboxd template"))
	}
	if c.AgentPort < 1 || c.AgentPort > 65535 {
		errs = append(errs, fmt.Errorf("agent_port must be in 1..65535, got %d", c.AgentPort))
	}
	return errors.Join(errs...)
}

func readToken(path, what string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return "", fmt.Errorf("read %s: %w", what, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("%s file %s is empty", what, path)
	}
	return token, nil
}
