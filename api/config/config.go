// Package config loads the cocoonbox-api configuration file.
package config

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

const (
	defaultListen    = "127.0.0.1:18200"
	defaultMCPListen = "127.0.0.1:18201"
	defaultAgentPort = 7443

	defaultStateInterval    = 15 * time.Minute
	defaultDeletedRetention = 7 * 24 * time.Hour
	defaultPresignTTL       = 10 * time.Minute
	defaultRegion           = "us-east-1"
	maxDeleteBatch          = 1000
	defaultMaxRunning       = 1

	defaultWakeSchedules = 3
	defaultWakeFloor     = time.Hour
	defaultMaxAwakeFor   = time.Hour

	defaultIdleHibernate = 30 * time.Minute

	defaultIdentityTimeout = 5 * time.Second

	schemeHTTPS = "https"

	defaultVCPUs     = 4
	defaultMemoryMiB = 8192
	defaultDiskGiB   = 40
)

// Flavor is the default box size.
type Flavor struct {
	VCPUs     uint32 `json:"vcpus"`
	MemoryMiB uint32 `json:"memory_mib"`
	DiskGiB   uint32 `json:"disk_gib"`
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
	Interval        Duration `json:"interval"`
	PresignTTL      Duration `json:"presign_ttl"`
	GC              GC       `json:"gc"`
	// ParkAfter parks a box hibernated this long: its machine and memory image go, and the next Ensure restores its saved state; "0s" turns parking off.
	ParkAfter Duration `json:"park_after"`
	// ConditionalWrites makes each index and timeline rewrite land only on the version it read; the store must honor If-Match.
	ConditionalWrites bool `json:"conditional_writes"`
}

// GC configures snapshot retention and chunk garbage collection; a zero Interval turns it off.
type GC struct {
	Interval Duration `json:"interval"`
	DryRun   bool     `json:"dry_run"`
	// DeleteBatch is the number of keys per DeleteObjects request.
	DeleteBatch int `json:"delete_batch"`
	// DeletedRetention keeps a deleted box's state this long after the first pass that finds it deleted.
	DeletedRetention Duration `json:"deleted_retention"`
}

func (g GC) validate(bucket string) error {
	var errs []error
	if g.Interval < 0 || (g.Interval > 0 && bucket == "") {
		errs = append(errs, errors.New("state.gc.interval must not be negative and needs state.bucket"))
	}
	if g.DeleteBatch < 1 || g.DeleteBatch > maxDeleteBatch {
		errs = append(errs, fmt.Errorf("state.gc.delete_batch must be in 1..%d, got %d", maxDeleteBatch, g.DeleteBatch))
	}
	if g.DeletedRetention < 0 {
		errs = append(errs, fmt.Errorf("state.gc.deleted_retention must not be negative, got %s", time.Duration(g.DeletedRetention)))
	}
	return errors.Join(errs...)
}

// Cluster configures the state api processes share in the users database.
type Cluster struct {
	// Ledger copies every machine, box head and operation into the database and verifies the copy every minute.
	Ledger bool `json:"ledger"`
	// Shared makes the database the truth for machines, box heads and operations, so api processes may serve together.
	Shared bool `json:"shared"`
	// Drain fails /healthz for this long after a stop signal before the api stops, so a balancer moves new calls to another api.
	Drain Duration `json:"drain"`
}

func (c Cluster) validate(conditionalWrites bool) error {
	var errs []error
	if c.Shared && !c.Ledger {
		errs = append(errs, errors.New("cluster.shared needs cluster.ledger"))
	}
	if c.Shared && !conditionalWrites {
		errs = append(errs, errors.New("cluster.shared needs state.conditional_writes"))
	}
	if c.Drain < 0 {
		errs = append(errs, fmt.Errorf("cluster.drain must not be negative, got %s", time.Duration(c.Drain)))
	}
	return errors.Join(errs...)
}

// Provision names the edge signing key, gw's admin API for per-user gw keys and the residential exit.
type Provision struct {
	SigningKeyFile string  `json:"signing_key_file"`
	GWAdmin        GWAdmin `json:"gw_admin"`
	// ResidentialUpstream is the residential forwarder's proxy URL; empty leaves every box's egress alone.
	ResidentialUpstream string `json:"residential_upstream"`
}

// GWAdmin names gw's admin API and the file holding its cocoonbox-boxes tenant admin token.
type GWAdmin struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
}

// Sandboxd names the sandboxd instance that runs the boxes and the file holding its root api token.
type Sandboxd struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
	// TenantEgressClass gives every user's tenant this egress class and turns on claims with the user's tenant; empty keeps root claims.
	TenantEgressClass string `json:"tenant_egress_class"`
}

// Accounts names the users database and the master key that seals each user's gw key, tenant token and state key.
type Accounts struct {
	// DSNFile holds the PostgreSQL connection string.
	DSNFile       string `json:"dsn_file"`
	MasterKeyFile string `json:"master_key_file"`
	// DefaultDailyCapMicros mirrors gw's user_daily_cost_quota_micros for the console; gw enforces it.
	DefaultDailyCapMicros int64 `json:"default_daily_cap_micros"`
}

// Connect configures the Connect broker (design §16); its OAuth clients come from GITHUB_APP_*, GOOGLE_*, HUBSPOT_*, SLACK_* and LINK_* env vars.
type Connect struct {
	// PublicURL is the api's public https origin; empty turns Connect off.
	PublicURL string `json:"public_url"`
	// BoxListen serves the box door (mode A) on this address of the host bridge, such as 10.203.0.1:18091; empty turns it off.
	BoxListen string `json:"box_listen"`
}

// OAuth makes the control MCP an OAuth 2.1 protected resource for agents the user approves in the console.
type OAuth struct {
	// PublicURL is the api's public https origin and the issuer; empty turns OAuth off.
	PublicURL string `json:"public_url"`
	// IdleHibernate hibernates a running box an agent created after this long without a client call; "0s" turns it off.
	IdleHibernate *Duration `json:"idle_hibernate"`
}

func (o OAuth) validate() error {
	var errs []error
	if u, err := url.Parse(o.PublicURL); o.PublicURL != "" && (err != nil || u.Scheme != schemeHTTPS || u.Host == "" || u.Path != "") {
		errs = append(errs, fmt.Errorf("oauth.public_url must be an https origin, got %q", o.PublicURL))
	}
	if *o.IdleHibernate < 0 {
		errs = append(errs, fmt.Errorf("oauth.idle_hibernate must not be negative, got %s", time.Duration(*o.IdleHibernate)))
	}
	return errors.Join(errs...)
}

// Identity names another user system whose token review endpoint resolves the bearer tokens cocoonbox does not issue; it alone decides who gets in.
type Identity struct {
	// URL takes POST {"token", "entry"} and {"credential", "entry"} and answers 200 with a review; empty keeps cocoonbox's own users alone.
	URL string `json:"url"`
	// TokenFile holds the api's bearer for url, read on every call; empty sends none.
	TokenFile string   `json:"token_file"`
	Timeout   Duration `json:"timeout"`
}

func (i Identity) validate() error {
	if i.URL == "" && i.TokenFile == "" {
		return nil
	}
	var errs []error
	if u, err := url.Parse(i.URL); err != nil || u.Scheme != schemeHTTPS || u.Host == "" {
		errs = append(errs, fmt.Errorf("identity.url must be an https url, got %q", i.URL))
	}
	if i.Timeout <= 0 {
		errs = append(errs, errors.New("identity.timeout must be positive"))
	}
	return errors.Join(errs...)
}

// Wake bounds the scheduled wakes of boxes (design §15.13).
type Wake struct {
	// MaxSchedules bounds the wake schedules of one user.
	MaxSchedules int `json:"max_schedules"`
	// Floor is the least time between two wakes of one box from hibernation.
	Floor       Duration `json:"floor"`
	MaxAwakeFor Duration `json:"max_awake_for"`
}

func (w Wake) validate() error {
	if w.MaxSchedules < 0 || w.Floor < 0 || w.MaxAwakeFor < Duration(time.Minute) {
		return fmt.Errorf("wake.max_schedules and wake.floor must not be negative and wake.max_awake_for must be at least 1m, got %d, %s, %s",
			w.MaxSchedules, time.Duration(w.Floor), time.Duration(w.MaxAwakeFor))
	}
	return nil
}

// Config is the on-disk configuration of cocoonbox-api.
type Config struct {
	Sandboxd     Sandboxd `json:"sandboxd"`
	Listen       string   `json:"listen"`
	MCPListen    string   `json:"mcp_listen"`
	Image        string   `json:"image"`
	RebaseImages []string `json:"rebase_images"`
	AgentPort    int      `json:"agent_port"`
	// Provision configures every box's identity file and gw key.
	Provision Provision `json:"provision"`
	Accounts  Accounts  `json:"accounts"`
	Flavor    Flavor    `json:"flavor"`
	Flavors   []Flavor  `json:"flavors"`
	// MaxRunningBoxes bounds how many boxes of one user run at once; Ensure and Create past it fail and name the running box.
	MaxRunningBoxes int `json:"max_running_boxes"`
	// UserIdleHibernate saves and hibernates a user's running box no client reached for this long; 0s (the default) leaves it up.
	UserIdleHibernate Duration `json:"user_idle_hibernate"`
	State             State    `json:"state"`
	Connect           Connect  `json:"connect"`
	// OperatorSocket serves the root CLI's box commands on this unix socket; empty turns them off.
	OperatorSocket string   `json:"operator_socket"`
	Wake           Wake     `json:"wake"`
	OAuth          OAuth    `json:"oauth"`
	Cluster        Cluster  `json:"cluster"`
	Identity       Identity `json:"identity"`
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

// DSN reads the users database connection string.
func (c *Config) DSN() (string, error) {
	return readToken(c.Accounts.DSNFile, "database dsn")
}

func (c *Config) applyDefaults() {
	c.Listen = cmp.Or(c.Listen, defaultListen)
	c.MCPListen = cmp.Or(c.MCPListen, defaultMCPListen)
	c.AgentPort = cmp.Or(c.AgentPort, defaultAgentPort)
	c.Flavor.VCPUs = cmp.Or(c.Flavor.VCPUs, defaultVCPUs)
	c.Flavor.MemoryMiB = cmp.Or(c.Flavor.MemoryMiB, defaultMemoryMiB)
	c.Flavor.DiskGiB = cmp.Or(c.Flavor.DiskGiB, defaultDiskGiB)
	c.State.Region = cmp.Or(c.State.Region, defaultRegion)
	c.State.Interval = cmp.Or(c.State.Interval, Duration(defaultStateInterval))
	c.State.PresignTTL = cmp.Or(c.State.PresignTTL, Duration(defaultPresignTTL))
	c.State.GC.DeleteBatch = cmp.Or(c.State.GC.DeleteBatch, maxDeleteBatch)
	c.State.GC.DeletedRetention = cmp.Or(c.State.GC.DeletedRetention, Duration(defaultDeletedRetention))
	c.MaxRunningBoxes = cmp.Or(c.MaxRunningBoxes, defaultMaxRunning)
	c.Wake.MaxSchedules = cmp.Or(c.Wake.MaxSchedules, defaultWakeSchedules)
	c.Wake.Floor = cmp.Or(c.Wake.Floor, Duration(defaultWakeFloor))
	c.Wake.MaxAwakeFor = cmp.Or(c.Wake.MaxAwakeFor, Duration(defaultMaxAwakeFor))
	c.Connect.PublicURL = strings.TrimSuffix(c.Connect.PublicURL, "/")
	c.OAuth.PublicURL = strings.TrimSuffix(c.OAuth.PublicURL, "/")
	c.Identity.Timeout = cmp.Or(c.Identity.Timeout, Duration(defaultIdentityTimeout))
	if c.OAuth.IdleHibernate == nil {
		c.OAuth.IdleHibernate = new(Duration(defaultIdleHibernate))
	}
}

// resolvePaths anchors relative file paths at dir, which lets systemd LoadCredential= hand over all files in one directory.
func (c *Config) resolvePaths(dir string) {
	for _, p := range []*string{&c.State.CredentialsFile, &c.Provision.SigningKeyFile, &c.Provision.GWAdmin.TokenFile, &c.Sandboxd.TokenFile, &c.Accounts.DSNFile, &c.Accounts.MasterKeyFile, &c.Identity.TokenFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	if c.Provision.SigningKeyFile == "" {
		errs = append(errs, errors.New("provision.signing_key_file is required: agents admit only edge-signed credentials"))
	}
	if c.Provision.GWAdmin.URL == "" || c.Provision.GWAdmin.TokenFile == "" {
		errs = append(errs, errors.New("provision.gw_admin.url and provision.gw_admin.token_file are required: every user has a gw key"))
	}
	if c.Accounts.DSNFile == "" || c.Accounts.MasterKeyFile == "" {
		errs = append(errs, errors.New("accounts.dsn_file and accounts.master_key_file are required"))
	}
	if c.State.Bucket != "" && (c.State.Endpoint == "" || c.State.CredentialsFile == "") {
		errs = append(errs, errors.New("state.endpoint and state.credentials_file are required with state.bucket"))
	}
	if c.UserIdleHibernate < 0 {
		errs = append(errs, fmt.Errorf("user_idle_hibernate must not be negative, got %s", time.Duration(c.UserIdleHibernate)))
	}
	if c.State.ParkAfter < 0 {
		errs = append(errs, fmt.Errorf("state.park_after must not be negative, got %s", time.Duration(c.State.ParkAfter)))
	}
	if c.Sandboxd.URL == "" || c.Sandboxd.TokenFile == "" {
		errs = append(errs, errors.New("sandboxd.url and sandboxd.token_file are required"))
	}
	if c.Image == "" {
		errs = append(errs, errors.New("image is required: it names the default sandboxd template"))
	}
	if c.MaxRunningBoxes < 1 {
		errs = append(errs, fmt.Errorf("max_running_boxes must be at least 1, got %d", c.MaxRunningBoxes))
	}
	if u, err := url.Parse(c.Connect.PublicURL); c.Connect.PublicURL != "" && (err != nil || u.Scheme != schemeHTTPS || u.Host == "" || strings.Trim(u.Path, "/") != "") {
		errs = append(errs, fmt.Errorf("connect.public_url must be an https origin, got %q", c.Connect.PublicURL))
	}
	if ip, err := netip.ParseAddrPort(c.Connect.BoxListen); c.Connect.BoxListen != "" && (err != nil || ip.Addr().IsUnspecified() || c.Connect.PublicURL == "") {
		errs = append(errs, fmt.Errorf("connect.box_listen must be an ip:port of one address, with connect.public_url set, got %q", c.Connect.BoxListen))
	}
	errs = append(errs, c.State.GC.validate(c.State.Bucket), c.Wake.validate(), c.OAuth.validate(), absolute("operator_socket", c.OperatorSocket))
	if c.AgentPort < 1 || c.AgentPort > 65535 {
		errs = append(errs, fmt.Errorf("agent_port must be in 1..65535, got %d", c.AgentPort))
	}
	errs = append(errs, c.Cluster.validate(c.State.ConditionalWrites), c.Identity.validate())
	return errors.Join(errs...)
}

// absolute refuses a relative path, which would resolve into the read-only credentials directory.
func absolute(key, path string) error {
	if path == "" || filepath.IsAbs(path) {
		return nil
	}
	return fmt.Errorf("%s must be an absolute path, got %q", key, path)
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
