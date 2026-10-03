package boxes

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/CMGS/cocoonbox/api/keyring"
)

const (
	manifestVersion = 1
	manifestLimit   = 256 << 20
	nonceSize       = 12

	kindFile    = "file"
	kindDir     = "dir"
	kindSymlink = "symlink"
)

var (
	errBadPath = errors.New("invalid box path")

	idLabel   = []byte("cocoonbox state chunk id v1")
	sealLabel = []byte("cocoonbox state seal v1")

	// stateRoots mirrors the agent's roots; manifests name a root and a path relative to it.
	stateRoots = []stateRoot{{name: "home", path: "/home/user"}, {name: "workspace", path: "/workspace"}}
)

// manifest is the agent's sealed list of a snapshot's files, as the api reads it.
type manifest struct {
	Version int            `json:"version"`
	Files   []manifestFile `json:"files"`
}

// selectPaths returns the files under paths and their distinct chunk ids in manifest order; a path that names nothing is an error.
func (m *manifest) selectPaths(paths []boxPath) ([]manifestFile, []string, error) {
	var picked []manifestFile
	var ids []string
	seen, found := map[string]bool{}, make([]bool, len(paths))
	for _, f := range m.Files {
		matched := false
		for i, bp := range paths {
			if bp.contains(f) {
				found[i], matched = true, true
			}
		}
		if !matched {
			continue
		}
		picked = append(picked, f)
		for _, id := range f.Chunks {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	for i, ok := range found {
		if !ok {
			return nil, nil, fmt.Errorf("%w: %s is not in the snapshot", errBadPath, paths[i])
		}
	}
	return picked, ids, nil
}

// tree indexes the manifest's entries without their chunks by their parent directory's box path; "" holds the roots.
func (m *manifest) tree() map[string][]manifestFile {
	dirs := map[string][]manifestFile{}
	for _, r := range stateRoots {
		dirs[""] = append(dirs[""], manifestFile{Root: r.name, Kind: kindDir})
	}
	for _, f := range m.Files {
		f.Chunks = nil
		parent := path.Dir(f.boxPath())
		dirs[parent] = append(dirs[parent], f)
	}
	return dirs
}

// manifestFile is one walked entry; Path is relative to its root.
type manifestFile struct {
	Root    string   `json:"root"`
	Path    string   `json:"path"`
	Kind    string   `json:"kind"`
	Mode    uint32   `json:"mode"`
	MtimeMs int64    `json:"mtime_ms"`
	Size    uint64   `json:"size"`
	Chunks  []string `json:"chunks,omitempty"`
}

func (f manifestFile) boxPath() string {
	for _, r := range stateRoots {
		if r.name == f.Root {
			return path.Join(r.path, f.Path)
		}
	}
	return path.Join("/", f.Root, f.Path)
}

type stateRoot struct {
	name string
	path string
}

// boxPath is a validated box path: a root and a path relative to it, empty for the root itself.
type boxPath struct {
	root stateRoot
	rel  string
}

func (bp boxPath) String() string {
	return path.Join(bp.root.path, bp.rel)
}

func (bp boxPath) contains(f manifestFile) bool {
	return f.Root == bp.root.name && (bp.rel == "" || f.Path == bp.rel || strings.HasPrefix(f.Path, bp.rel+"/"))
}

// openManifest reads the manifest of snapshot id of p's box, rebuilding it from a delta when needed, and decodes it with key.
func (l *stateLayer) openManifest(ctx context.Context, p statePath, key keyring.Key, id string) (*manifest, error) {
	plain, err := l.manifestPlain(ctx, p, key, id)
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, fmt.Errorf("decode manifest %s: %w", id, err)
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("manifest %s has version %d", id, m.Version)
	}
	return &m, nil
}

// parseBoxPath accepts an absolute path under a state root without ".", ".." or empty components.
func parseBoxPath(p string) (boxPath, error) {
	parts := strings.Split(strings.TrimSuffix(p, "/"), "/")
	if len(parts) < 2 || parts[0] != "" {
		return boxPath{}, fmt.Errorf("%w: %q is not absolute", errBadPath, p)
	}
	for _, part := range parts[1:] {
		if part == "" || part == "." || part == ".." {
			return boxPath{}, fmt.Errorf("%w: %q", errBadPath, p)
		}
	}
	clean := strings.Join(parts, "/")
	for _, r := range stateRoots {
		if rel, ok := strings.CutPrefix(clean, r.path); ok && (rel == "" || rel[0] == '/') {
			return boxPath{root: r, rel: strings.TrimPrefix(rel, "/")}, nil
		}
	}
	return boxPath{}, fmt.Errorf("%w: %q is not under /home/user or /workspace", errBadPath, p)
}

// openSealed reverses the agent's seal: AES-256-GCM bound to id, zstd, and the keyed hash of the plaintext equal to id.
func openSealed(secret []byte, id string, sealed []byte, limit int64) ([]byte, error) {
	plain, err := openObject(secret, id, sealed, limit)
	if err != nil {
		return nil, err
	}
	if !matchesID(secret, id, plain) {
		return nil, fmt.Errorf("object %s does not match its id", id)
	}
	return plain, nil
}

// openObject decrypts sealed with aad as associated data and decompresses at most limit bytes.
func openObject(secret []byte, aad string, sealed []byte, limit int64) ([]byte, error) {
	gcm, err := sealCipher(secret)
	if err != nil {
		return nil, err
	}
	if len(sealed) < nonceSize {
		return nil, fmt.Errorf("object %s is truncated", aad)
	}
	packed, err := gcm.Open(nil, sealed[:nonceSize], sealed[nonceSize:], []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("object %s failed authentication", aad)
	}
	dec, err := zstd.NewReader(bytes.NewReader(packed), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	plain, err := io.ReadAll(io.LimitReader(dec, limit+1))
	if err != nil {
		return nil, fmt.Errorf("decompress %s: %w", aad, err)
	}
	if int64(len(plain)) > limit {
		return nil, fmt.Errorf("object %s exceeds %d bytes", aad, limit)
	}
	return plain, nil
}

// sealObject seals plain as the agent does: zstd, then AES-256-GCM with aad as associated data.
func sealObject(secret []byte, aad string, plain []byte) ([]byte, error) {
	gcm, err := sealCipher(secret)
	if err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	packed := enc.EncodeAll(plain, nil)
	if err := enc.Close(); err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, packed, []byte(aad)), nil
}

func sealCipher(secret []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(mac(secret, sealLabel))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// matchesID reports whether id is the keyed hash of plain.
func matchesID(secret []byte, id string, plain []byte) bool {
	want, err := hex.DecodeString(id)
	return err == nil && hmac.Equal(mac(mac(secret, idLabel), plain), want)
}

func mac(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}
