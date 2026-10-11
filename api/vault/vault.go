// Package vault keeps each user's API keys sealed and puts them on the user's boxes as egress credentials: the box holds a
// placeholder, and sandboxd swaps in the value on requests to the key's hosts.
package vault

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/keyring"
	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	// EnvPrefix starts the claim env name of every key's host-only entry.
	EnvPrefix = "CBV_"
	// GuestEnv is the guest entry that lists the keys' names, placeholders, hosts and send-as for the image.
	GuestEnv = "COCOONBOX_VAULT"

	// ActorApp and ActorConsole name where a change comes from in the audit.
	ActorApp     = "app"
	ActorConsole = "console"

	opCreate = "create"
	opRotate = "rotate"
	opDelete = "delete"

	maxKeys          = 32
	maxHosts         = 8
	maxHostLen       = 253
	maxValue         = 1024
	maxTemplate      = 128
	maxPrefix        = 16
	hostBudget       = 40 << 10
	guestBudget      = 7 << 10
	hintMinLen       = 12
	hintLen          = 4
	fingerprintLen   = 8
	idBytes          = 6
	placeholderBytes = 16
	digestLen        = 16
	auditRetention   = 90 * 24 * time.Hour

	sealInfo        = "cocoonbox vault value"
	idPrefix        = "vs_"
	placeholderMark = "cbv_"
	slot            = "{}"
	basicPrefix     = "Basic "
	defaultHeader   = "Authorization"
	defaultTemplate = "Bearer {}"
	recordFormat    = "v1"
)

var (
	errInvalid = errors.New("invalid key")
	errExists  = errors.New("key exists")
	errNoKey   = errors.New("no such key")
	errQuota   = errors.New("too many keys")

	namePattern  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	queryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	hostPattern  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	vendorPrefix = regexp.MustCompile(`^([a-z]{1,6}[0-9]{0,2}[-_]){1,3}`)

	reservedNames = []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "PWD", "OLDPWD", "TERM", "LANG", "DISPLAY", "XAUTHORITY", "TMPDIR", "HOSTNAME",
		"IFS", "PS1", "PS2", "PS4", "ENV", "PAGER", "EDITOR", "NO_COLOR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "FTP_PROXY",
		"REQUESTS_CA_BUNDLE",
	}
	reservedPrefixes = []string{"GW_", "COCOONBOX_", "CB_", EnvPrefix, "SSL_", "NODE_", "LC_", "LD_", "XDG_", "DBUS_", "BASH_", "GIT_", "SUDO_"}
	refusedHeaders   = []string{
		"Host", "Content-Length", "Cookie", "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
		"TE", "Trailer", "Transfer-Encoding", "Upgrade",
	}
	howEscaper = strings.NewReplacer("%", "%25", " ", "%20", ";", "%3B")
)

// Options configures the vault.
type Options struct {
	DSN string
	// Keys gives each user's state key; values are sealed under a key derived from it.
	Keys keyring.Keyring
}

// ChangeFunc runs after a key of owner is added, rotated or deleted.
type ChangeFunc func(ctx context.Context, owner string)

type actorKey struct{}

// Service stores the keys and serves Keys.
type Service struct {
	controlv1.UnimplementedKeysServer

	store    *store
	seal     *sealer
	onChange ChangeFunc
}

// Open connects to the database, which must hold the users table, and applies the vault migrations.
func Open(ctx context.Context, opts Options) (*Service, error) {
	st, err := openStore(ctx, opts.DSN)
	if err != nil {
		return nil, err
	}
	return &Service{store: st, seal: &sealer{keys: opts.Keys}}, nil
}

// Close releases the database pool.
func (s *Service) Close() { s.store.close() }

// OnChange registers fn, which runs synchronously in the call that changed the key.
func (s *Service) OnChange(fn ChangeFunc) {
	s.onChange = fn
}

// Digests returns a digest of each owner's keys and their versions; an owner without keys has none.
func (s *Service) Digests(ctx context.Context) (map[string]string, error) {
	versions, err := s.store.versions(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(versions))
	for owner, v := range versions {
		out[owner] = digest(v)
	}
	return out, nil
}

// Credentials returns what owner's keys put on a claim, with the digest of the keys it holds.
func (s *Service) Credentials(ctx context.Context, owner string) (machines.Credentials, string, error) {
	list, err := s.store.secrets(ctx, owner)
	if err != nil {
		return machines.Credentials{}, "", err
	}
	out := machines.Credentials{Secrets: map[string]*machines.Secret{}, Flags: map[string]string{GuestEnv: ""}, Prune: EnvPrefix}
	records, versions := make([]string, 0, len(list)), make([]string, 0, len(list))
	for _, sec := range list {
		value, err := s.seal.open(ctx, owner, sec.sealed, valueData(sec.id, owner))
		if err != nil {
			return machines.Credentials{}, "", fmt.Errorf("open key %s: %w", sec.name, err)
		}
		out.Secrets[envName(sec.id)] = sec.entry(value)
		records = append(records, sec.record())
		versions = append(versions, fmt.Sprintf("%s:%d", sec.id, sec.version))
	}
	out.Flags[GuestEnv] = strings.Join(records, " ")
	return out, digest(versions), nil
}

func (s *Service) changed(ctx context.Context, owner string) {
	if s.onChange != nil {
		s.onChange(ctx, owner)
	}
}

// secret is one stored key; sealed holds its value.
type secret struct {
	id          string
	owner       string
	name        string
	hosts       []string
	header      string
	template    string
	query       string
	placeholder string
	sealed      []byte
	hint        string
	fingerprint string
	valueLen    int
	version     int
	createdAt   time.Time
	updatedAt   time.Time
}

// entry is sec's host-only claim credential for value: the placeholder gates every entry.
func (sec *secret) entry(value string) *machines.Secret {
	if sec.query != "" {
		return &machines.Secret{Value: value, Hosts: sec.hosts, Query: sec.query, Placeholder: sec.placeholder}
	}
	return &machines.Secret{Value: render(sec.template, value), Hosts: sec.hosts, Header: sec.header, Placeholder: render(sec.template, sec.placeholder)}
}

// record is sec's word in GuestEnv: NAME=placeholder;host,host;how, with how's spaces, semicolons and percent signs escaped.
func (sec *secret) record() string {
	how := "?" + sec.query + "=" + slot
	if sec.query == "" {
		how = sec.header + ": " + sec.template
	}
	return sec.name + "=" + sec.placeholder + ";" + strings.Join(sec.hosts, ",") + ";" + howEscaper.Replace(how)
}

// size is what sec takes of a claim's env: its host-only entry, and its record in GuestEnv.
func (sec *secret) size() (int, int) {
	n := len(envName(sec.id)) + len(sec.header) + len(sec.query)
	for _, h := range sec.hosts {
		n += len(h)
	}
	if sec.query != "" {
		n += sec.valueLen + len(sec.placeholder)
	} else {
		n += renderedLen(sec.template, sec.valueLen) + renderedLen(sec.template, len(sec.placeholder))
	}
	return n, len(sec.record()) + 1
}

func (sec *secret) key() *controlv1.Key {
	return &controlv1.Key{
		Name: sec.name, Hosts: sec.hosts, Header: sec.header, Template: sec.template, Query: sec.query, Placeholder: sec.placeholder,
		Hint: sec.hint, CreatedAtMs: sec.createdAt.UnixMilli(), UpdatedAtMs: sec.updatedAt.UnixMilli(),
	}
}

// WithActor marks ctx's key changes as coming from actor in the audit.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// fits refuses a set of keys that a claim's env cannot hold.
func fits(list []*secret) error {
	if len(list) > maxKeys {
		return fmt.Errorf("%w: %d keys is the most a user may have", errQuota, maxKeys)
	}
	hostBytes, guestBytes := 0, 0
	for _, sec := range list {
		h, g := sec.size()
		hostBytes, guestBytes = hostBytes+h, guestBytes+g
	}
	if hostBytes > hostBudget || guestBytes > guestBudget {
		return fmt.Errorf("%w: the keys' values, hosts and names exceed what a box holds; use fewer or shorter ones", errQuota)
	}
	return nil
}

func actorOf(ctx context.Context) string {
	if actor, ok := ctx.Value(actorKey{}).(string); ok {
		return actor
	}
	return ActorApp
}

func renderedLen(template string, n int) int {
	n += len(template) - len(slot)
	if strings.HasPrefix(template, basicPrefix) {
		return len(basicPrefix) + base64.StdEncoding.EncodedLen(n-len(basicPrefix))
	}
	return n
}

func digest(versions []string) string {
	if len(versions) == 0 {
		return ""
	}
	slices.Sort(versions)
	sum := sha256.Sum256([]byte(recordFormat + "|" + strings.Join(versions, ",")))
	return hex.EncodeToString(sum[:])[:digestLen]
}

func envName(id string) string {
	return EnvPrefix + strings.TrimPrefix(id, idPrefix)
}

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}
