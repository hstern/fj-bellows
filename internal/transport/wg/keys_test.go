package wg

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestLoadOrGenerateKey_GeneratesOnFirstCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wg-private-key")

	k, err := LoadOrGenerateKey(path)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if k == (wgtypes.Key{}) {
		t.Fatal("generated zero key")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perms = %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrGenerateKey_StableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wg-private-key")

	first, err := LoadOrGenerateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrGenerateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("second call generated a new key; want stable load")
	}
}

func TestLoadOrGenerateKey_RejectsLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wg-private-key")
	// First create with the right perms so we have a valid file.
	if _, err := LoadOrGenerateKey(path); err != nil {
		t.Fatal(err)
	}
	// Now widen the perms — group-readable is enough to trip the check.
	//nolint:gosec // G302: deliberately loose; this test verifies the check rejects it.
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrGenerateKey(path)
	if err == nil {
		t.Fatal("loose perms should error")
	}
	if !contains(err.Error(), "too-permissive") {
		t.Errorf("error message = %q; want substring 'too-permissive'", err)
	}
}

func TestLoadOrGenerateKey_RejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wg-private-key")
	if err := os.WriteFile(path, []byte("not base64 garbage!"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrGenerateKey(path)
	if err == nil {
		t.Fatal("corrupt key should error")
	}
}

func TestDecodeKeyRoundTrip(t *testing.T) {
	// A real Curve25519 private key (32 random bytes) round-trips
	// through EncodeKey / DecodeKey unchanged.
	gen, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	encoded := EncodeKey(gen)
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		t.Errorf("EncodeKey output not valid base64: %v", err)
	}
	parsed, err := DecodeKey(encoded)
	if err != nil {
		t.Fatalf("DecodeKey: %v", err)
	}
	if parsed != gen {
		t.Errorf("round-trip lost data: %v != %v", parsed, gen)
	}
}

func TestDecodeKey_RejectsWrongLength(t *testing.T) {
	// 16 bytes of valid base64 → wrong WG key length.
	s := base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := DecodeKey(s); err == nil {
		t.Fatal("wrong-length key should error")
	}
}

func TestDecodeKey_EmptyRejected(t *testing.T) {
	if _, err := DecodeKey(""); err == nil {
		t.Fatal("empty key should error")
	}
	if _, err := DecodeKey("   \n\t"); err == nil {
		t.Fatal("whitespace-only key should error")
	}
}

func TestPublicKeyOf(t *testing.T) {
	priv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := PublicKeyOf(priv)
	// Public key derivation is deterministic; same private always
	// produces the same public.
	if PublicKeyOf(priv) != pub {
		t.Fatal("PublicKeyOf is non-deterministic")
	}
	// Public key is not the same as private (sanity).
	if pub == priv {
		t.Fatal("PublicKey == PrivateKey; derivation didn't fire")
	}
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
