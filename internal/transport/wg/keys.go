package wg

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// LoadOrGenerateKey returns the WireGuard private key at path,
// generating + persisting a fresh Curve25519 key on first start.
//
// On-disk format: a single base64-encoded 32-byte private key, no
// trailing newline. The file is created with 0600 permissions; if the
// existing file is world- or group-readable LoadOrGenerateKey errors
// rather than tighten silently. Operators rotate by removing the file
// before next start.
func LoadOrGenerateKey(path string) (wgtypes.Key, error) {
	if path == "" {
		return wgtypes.Key{}, errors.New("wg: private_key_file is required")
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return generateAndPersist(path)
	case err != nil:
		return wgtypes.Key{}, fmt.Errorf("stat private key: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return wgtypes.Key{}, fmt.Errorf(
			"wg private key %s has too-permissive mode %v; expected 0600",
			path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("read private key: %w", err)
	}
	key, err := decodeKey(string(raw))
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("parse private key %s: %w", path, err)
	}
	return key, nil
}

// PublicKeyOf is sugar over the wgtypes.Key.PublicKey() method —
// exposed here so callers can stay in this package's namespace.
func PublicKeyOf(priv wgtypes.Key) wgtypes.Key {
	return priv.PublicKey()
}

// EncodeKey returns the base64 representation WireGuard tooling uses.
// Provided so callers can render a public key for operator paste-in
// without importing wgctrl/wgtypes themselves.
func EncodeKey(k wgtypes.Key) string {
	return k.String()
}

// DecodeKey parses a base64-encoded WireGuard key (private or public).
// Exposed so the caller can validate operator-pasted peer keys before
// passing them to New.
func DecodeKey(s string) (wgtypes.Key, error) {
	return decodeKey(s)
}

func generateAndPersist(path string) (wgtypes.Key, error) {
	priv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("generate private key: %w", err)
	}
	// O_EXCL guards against two daemons racing to write the same file.
	//nolint:gosec // G304: path is the operator-supplied config field, not user input.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("create private key file: %w", err)
	}
	if _, err := f.WriteString(priv.String()); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return wgtypes.Key{}, fmt.Errorf("write private key: %w", err)
	}
	if err := f.Close(); err != nil {
		return wgtypes.Key{}, fmt.Errorf("close private key file: %w", err)
	}
	return priv, nil
}

func decodeKey(s string) (wgtypes.Key, error) {
	trimmed := trimWhitespace(s)
	if trimmed == "" {
		return wgtypes.Key{}, errors.New("empty key")
	}
	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("base64: %w", err)
	}
	if len(raw) != wgtypes.KeyLen {
		return wgtypes.Key{}, fmt.Errorf("wrong length %d (want %d)", len(raw), wgtypes.KeyLen)
	}
	return wgtypes.NewKey(raw)
}

func trimWhitespace(s string) string {
	start := 0
	for start < len(s) && isSpace(s[start]) {
		start++
	}
	end := len(s)
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
