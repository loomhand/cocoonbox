// Package flows keeps each user's flow library: the GUI flows and scripts that cbplan boxes sync through the
// box door, and that the owner lists, exports, imports and deletes through cocoonbox.control.v1.Flows.
package flows

import (
	"cmp"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/projecteru2/core/log"

	"github.com/CMGS/cocoonbox/api/keyring"
	"github.com/CMGS/cocoonbox/api/store"
)

const (
	// MaxContent is the largest flow in bytes.
	MaxContent = 128 << 10
	// MaxFlows is the most flows one library holds.
	MaxFlows = 200
	// MaxBytes is the most content one library holds, so one Export answer stays under gRPC's 4 MiB default.
	MaxBytes = 3 << 20

	sealInfo  = "cocoonbox flows"
	objectLen = 16
)

var (
	// ErrInvalid reports a name or content the library refuses.
	ErrInvalid = errors.New("invalid flow")
	// ErrNotFound reports a name the library lacks.
	ErrNotFound = errors.New("flow not found")
	// ErrFull reports a write past MaxFlows or MaxBytes.
	ErrFull = errors.New("flow library full")

	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
)

// Entry is one flow of a library without its content.
type Entry struct {
	Name      string `json:"name"`
	Rev       uint64 `json:"rev"`
	SHA256    string `json:"sha256"`
	Size      int    `json:"size"`
	SavedAtMs int64  `json:"saved_at_ms"`
	BoxID     string `json:"box_id,omitempty"`
	Image     string `json:"image,omitempty"`
}

// Origin names the writer of a flow: a box and its image, or an import with the image it names.
type Origin struct {
	BoxID string
	Image string
}

// Library keeps every user's flows in the state store, sealed under a key derived from the user's state key.
type Library struct {
	store store.Store
	keys  keyring.Keyring

	mu    sync.Mutex
	users map[string]*sync.Mutex
}

// NewLibrary keeps flows in st under each user's prefix.
func NewLibrary(st store.Store, keys keyring.Keyring) *Library {
	return &Library{store: st, keys: keys, users: map[string]*sync.Mutex{}}
}

// List returns user's library rev and flows, newest first.
func (l *Library) List(ctx context.Context, user string) (uint64, []Entry, error) {
	unlock := l.lock(user)
	defer unlock()
	idx, err := l.index(ctx, user)
	if err != nil {
		return 0, nil, err
	}
	entries := make([]Entry, len(idx.Flows))
	for i, f := range idx.Flows {
		entries[i] = f.Entry
	}
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Or(cmp.Compare(b.Rev, a.Rev), strings.Compare(a.Name, b.Name)) })
	return idx.Rev, entries, nil
}

// Get returns one flow and its content.
func (l *Library) Get(ctx context.Context, user, name string) (Entry, []byte, error) {
	unlock := l.lock(user)
	defer unlock()
	idx, err := l.index(ctx, user)
	if err != nil {
		return Entry{}, nil, err
	}
	i := idx.find(name)
	if i < 0 {
		return Entry{}, nil, ErrNotFound
	}
	f := idx.Flows[i]
	sealed, err := l.store.Get(ctx, objectKey(user, f.Object))
	if err != nil {
		return Entry{}, nil, fmt.Errorf("read flow object: %w", err)
	}
	content, err := l.open(ctx, user, sealed, objectData(user, f.Object))
	if err != nil {
		return Entry{}, nil, err
	}
	return f.Entry, content, nil
}

// Put writes a flow, the last write winning; content equal to the stored one changes nothing.
func (l *Library) Put(ctx context.Context, user, name string, content []byte, o Origin) (Entry, error) {
	if err := Check(name, content); err != nil {
		return Entry{}, err
	}
	unlock := l.lock(user)
	defer unlock()
	idx, err := l.index(ctx, user)
	if err != nil {
		return Entry{}, err
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	i := idx.find(name)
	if i >= 0 && idx.Flows[i].SHA256 == digest {
		return idx.Flows[i].Entry, nil
	}
	count, total := len(idx.Flows), len(content)
	for _, f := range idx.Flows {
		total += f.Size
	}
	if i >= 0 {
		count--
		total -= idx.Flows[i].Size
	}
	if count >= MaxFlows || total > MaxBytes {
		return Entry{}, fmt.Errorf("%w: at most %d flows and %d bytes", ErrFull, MaxFlows, MaxBytes)
	}
	object := randomHex(objectLen)
	sealed, err := l.seal(ctx, user, content, objectData(user, object))
	if err != nil {
		return Entry{}, err
	}
	if err := l.store.Put(ctx, objectKey(user, object), sealed); err != nil {
		return Entry{}, fmt.Errorf("write flow object: %w", err)
	}
	idx.Rev++
	f := stored{
		Name: name, Rev: idx.Rev, SHA256: digest, Size: len(content), SavedAtMs: time.Now().UnixMilli(),
		BoxID: o.BoxID, Image: o.Image, Object: object,
	}
	var replaced string
	if i >= 0 {
		replaced, idx.Flows[i] = idx.Flows[i].Object, f
	} else {
		idx.Flows = append(idx.Flows, f)
	}
	if err := l.writeIndex(ctx, user, idx); err != nil {
		return Entry{}, err
	}
	if replaced != "" {
		l.dropObjects(ctx, user, []string{replaced})
	}
	return f.Entry, nil
}

// Delete removes the named flows and returns the names the library held.
func (l *Library) Delete(ctx context.Context, user string, names []string) ([]string, error) {
	unlock := l.lock(user)
	defer unlock()
	idx, err := l.index(ctx, user)
	if err != nil {
		return nil, err
	}
	var deleted, objects []string
	idx.Flows = slices.DeleteFunc(idx.Flows, func(f stored) bool {
		if !slices.Contains(names, f.Name) {
			return false
		}
		deleted, objects = append(deleted, f.Name), append(objects, f.Object)
		return true
	})
	if len(deleted) == 0 {
		return nil, nil
	}
	idx.Rev++
	if err := l.writeIndex(ctx, user, idx); err != nil {
		return nil, err
	}
	l.dropObjects(ctx, user, objects)
	return deleted, nil
}

func (l *Library) lock(user string) func() {
	l.mu.Lock()
	m := l.users[user]
	if m == nil {
		m = &sync.Mutex{}
		l.users[user] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (l *Library) index(ctx context.Context, user string) (*index, error) {
	sealed, err := l.store.Get(ctx, indexKey(user))
	if errors.Is(err, store.ErrNotFound) {
		return &index{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read flow index: %w", err)
	}
	plain, err := l.open(ctx, user, sealed, indexData(user))
	if err != nil {
		return nil, err
	}
	idx := &index{}
	if err := json.Unmarshal(plain, idx); err != nil {
		return nil, fmt.Errorf("decode flow index: %w", err)
	}
	return idx, nil
}

func (l *Library) writeIndex(ctx context.Context, user string, idx *index) error {
	plain, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	sealed, err := l.seal(ctx, user, plain, indexData(user))
	if err != nil {
		return err
	}
	if err := l.store.Put(ctx, indexKey(user), sealed); err != nil {
		return fmt.Errorf("write flow index: %w", err)
	}
	return nil
}

// dropObjects removes objects the index no longer names; a failure leaves them under the user's prefix.
func (l *Library) dropObjects(ctx context.Context, user string, objects []string) {
	keys := make([]string, len(objects))
	for i, o := range objects {
		keys[i] = objectKey(user, o)
	}
	if err := l.store.Delete(ctx, keys); err != nil {
		log.WithFunc("flows.Library.dropObjects").Warnf(ctx, "remove %d replaced flow objects of %s: %v", len(keys), user, err)
	}
}

func (l *Library) seal(ctx context.Context, user string, plain []byte, ad string) ([]byte, error) {
	aead, err := l.aead(ctx, user)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	_, _ = rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, []byte(ad)), nil
}

func (l *Library) open(ctx context.Context, user string, sealed []byte, ad string) ([]byte, error) {
	aead, err := l.aead(ctx, user)
	if err != nil {
		return nil, err
	}
	n := aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed flow is truncated")
	}
	plain, err := aead.Open(nil, sealed[:n], sealed[n:], []byte(ad))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", ad, err)
	}
	return plain, nil
}

func (l *Library) aead(ctx context.Context, user string) (cipher.AEAD, error) {
	k, err := l.keys.StateKey(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("state key of %s: %w", user, err)
	}
	key, err := hkdf.Key(sha256.New, k.Secret, nil, sealInfo, keyring.KeySize)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Check refuses a name that is no plain .json or .mts file name, and content that is too large or not UTF-8.
func Check(name string, content []byte) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: name %q must be a .json or .mts file name of letters, digits, '.', '_' and '-'", ErrInvalid, name)
	}
	if len(content) > MaxContent {
		return fmt.Errorf("%w: %s holds %d bytes, more than %d", ErrFull, name, len(content), MaxContent)
	}
	if !utf8.Valid(content) {
		return fmt.Errorf("%w: %s is not UTF-8 text", ErrInvalid, name)
	}
	return nil
}

// ValidName reports whether name is a flow file name the library takes.
func ValidName(name string) bool {
	return namePattern.MatchString(name) && (strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".mts"))
}

// index is a library as stored: every flow with the object that holds its content.
type index struct {
	Rev   uint64   `json:"rev"`
	Flows []stored `json:"flows"`
}

func (idx *index) find(name string) int {
	return slices.IndexFunc(idx.Flows, func(f stored) bool { return f.Name == name })
}

type stored struct {
	Entry
	Object string `json:"object"`
}

func indexKey(user string) string           { return user + "/flows/index" }
func objectKey(user, object string) string  { return user + "/flows/" + object }
func indexData(user string) string          { return "flows|" + user + "|index" }
func objectData(user, object string) string { return "flows|" + user + "|" + object }

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}
