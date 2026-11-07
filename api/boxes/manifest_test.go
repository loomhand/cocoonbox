package boxes

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestTheContractFixtureOpensSplitsAndSealsLikeTheAgent(t *testing.T) {
	raw, err := os.ReadFile("../../docs/fixtures/state-v1.json")
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	type object struct {
		ID        string `json:"id"`
		Plain     string `json:"plain"`
		PlainB64  string `json:"plain_b64"`
		SealedB64 string `json:"sealed_b64"`
	}
	var f struct {
		KeyHex   string `json:"key_hex"`
		Manifest object `json:"manifest"`
		Chunk    object `json:"chunk"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode the fixture: %v", err)
	}
	secret, _ := hex.DecodeString(f.KeyHex)
	chunkPlain, _ := base64.StdEncoding.DecodeString(f.Chunk.PlainB64)
	for _, o := range []struct {
		obj   object
		plain []byte
	}{{f.Manifest, []byte(f.Manifest.Plain)}, {f.Chunk, chunkPlain}} {
		sealed, _ := base64.StdEncoding.DecodeString(o.obj.SealedB64)
		got, err := openSealed(secret, o.obj.ID, sealed, manifestLimit)
		if err != nil || !bytes.Equal(got, o.plain) {
			t.Fatalf("open %s = %q, %v", o.obj.ID, got, err)
		}
		resealed, err := sealObject(secret, o.obj.ID, o.plain)
		if err != nil {
			t.Fatalf("seal %s: %v", o.obj.ID, err)
		}
		if again, err := openSealed(secret, o.obj.ID, resealed, manifestLimit); err != nil || !bytes.Equal(again, o.plain) {
			t.Fatalf("reopen %s = %q, %v", o.obj.ID, again, err)
		}
	}
	plain := []byte(f.Manifest.Plain)
	var m manifest
	if err := json.Unmarshal(plain, &m); err != nil || m.Version != manifestVersion || len(m.Files) != 3 {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	if got := m.Files[2]; got.fullPath() != "/workspace/notes.txt" || got.Kind != kindFile || !slices.Equal(got.Chunks, []string{f.Chunk.ID}) {
		t.Errorf("file = %+v", got)
	}
	parts, err := splitManifest(plain)
	if err != nil || len(parts.files) != 3 || !bytes.Equal(parts.join(), plain) {
		t.Errorf("split = %d files, %v", len(parts.files), err)
	}
}

func TestOpenSealedRejectsTamperingAndOtherIDs(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	id, sealed := sealManifest(secret, sampleFiles())
	if _, err := openSealed(secret, id, sealed, manifestLimit); err != nil {
		t.Fatalf("open: %v", err)
	}
	flipped := slices.Clone(sealed)
	flipped[len(flipped)-1] ^= 1
	if _, err := openSealed(secret, id, flipped, manifestLimit); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Errorf("tampered object: %v", err)
	}
	if _, err := openSealed(secret, snapshotID(1), sealed, manifestLimit); err == nil {
		t.Error("an object opened under another id")
	}
	if _, err := openSealed([]byte("another key, also 32 bytes long."), id, sealed, manifestLimit); err == nil {
		t.Error("an object opened under another key")
	}
	if _, err := openSealed(secret, id, sealed, 10); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized object: %v", err)
	}
}

func sealManifest(secret []byte, files []manifestFile) (string, []byte) {
	plain, err := json.Marshal(manifest{Version: manifestVersion, Files: files})
	if err != nil {
		panic(err)
	}
	id := hex.EncodeToString(mac(mac(secret, idLabel), plain))
	sealed, err := sealObject(secret, id, plain)
	if err != nil {
		panic(err)
	}
	return id, sealed
}
