package linode

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// cacheCertPair holds the fjb-managed CA and the server certificate it
// signed for the cache VM. The CA cert is the trust anchor distributed
// to workers (via worker cloud-init); the server cert + key live on the
// cache VM. The CA is persisted to disk so adopt-existing on daemon
// restart keeps the cert chain consistent — workers booted by the new
// daemon trust the same CA the adopted cache's cert was signed by.
type cacheCertPair struct {
	CACertPEM     []byte // distributed to workers as trust anchor
	ServerCertPEM []byte // baked into cache VM cloud-init
	ServerKeyPEM  []byte // baked into cache VM cloud-init
}

// PEM filenames inside cache.tls.ca_dir. Stable so daemon restarts can
// find a previously-persisted CA.
const (
	caCertFilename = "ca-cert.pem"
	caKeyFilename  = "ca-key.pem"
)

// loadOrGenerateCertPair returns the cert chain for the cache VM. It
// load-or-generates the CA (persisting fresh CAs to caDir so the next
// daemon lifetime can adopt them), then signs a fresh server cert with
// the resolved CA. The second return reports whether the CA was
// freshly generated this call — callers use this to decide whether an
// adopted cache VM is still trusted (an existing cache VM signed by a
// since-vanished CA is a misconfiguration the operator must reconcile).
func loadOrGenerateCertPair(caDir, cacheHost string) (cacheCertPair, bool, error) {
	if cacheHost == "" {
		return cacheCertPair{}, false, errors.New("loadOrGenerateCertPair: cacheHost is required")
	}
	if caDir == "" {
		return cacheCertPair{}, false, errors.New("loadOrGenerateCertPair: caDir is required")
	}
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		return cacheCertPair{}, false, fmt.Errorf("mkdir CA dir %q: %w", caDir, err)
	}
	caCert, caKey, caCertPEM, err := loadCA(caDir)
	if err != nil {
		return cacheCertPair{}, false, fmt.Errorf("load CA: %w", err)
	}
	freshCA := false
	if caCert == nil {
		caCert, caKey, caCertPEM, err = generateAndPersistCA(caDir)
		if err != nil {
			return cacheCertPair{}, false, fmt.Errorf("generate CA: %w", err)
		}
		freshCA = true
	}
	srvCertPEM, srvKeyPEM, err := newServerCert(cacheHost, caCert, caKey)
	if err != nil {
		return cacheCertPair{}, false, fmt.Errorf("server cert: %w", err)
	}
	return cacheCertPair{
		CACertPEM:     caCertPEM,
		ServerCertPEM: srvCertPEM,
		ServerKeyPEM:  srvKeyPEM,
	}, freshCA, nil
}

// loadCA reads ca-cert.pem + ca-key.pem from dir. Returns all-nil
// (with no error) when both files are missing — the load-or-generate
// caller then falls through to fresh generation. Treats partial
// state (only one file present) as an error — that's a corrupted
// dir we should not silently mask.
func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	certPath := filepath.Join(dir, caCertFilename)
	keyPath := filepath.Join(dir, caKeyFilename)
	certExists, certErr := pathExists(certPath)
	if certErr != nil {
		return nil, nil, nil, certErr
	}
	keyExists, keyErr := pathExists(keyPath)
	if keyErr != nil {
		return nil, nil, nil, keyErr
	}
	switch {
	case !certExists && !keyExists:
		return nil, nil, nil, nil
	case certExists != keyExists:
		return nil, nil, nil, fmt.Errorf("partial CA state in %q: cert present=%v, key present=%v — delete %q and let fjb regenerate",
			dir, certExists, keyExists, dir)
	}
	certPEM, err := os.ReadFile(certPath) //nolint:gosec // G304: certPath is dir + constant filename, dir is operator-supplied config not user input
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read %q: %w", certPath, err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, nil, fmt.Errorf("decode %q: not PEM", certPath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse %q: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath) //nolint:gosec // G304: keyPath is dir + constant filename, dir is operator-supplied config not user input
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read %q: %w", keyPath, err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, nil, fmt.Errorf("decode %q: not PEM", keyPath)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse %q: %w", keyPath, err)
	}
	return cert, key, certPEM, nil
}

// generateAndPersistCA writes a fresh CA to disk and returns it. Files
// are written with 0600 (key) / 0644 (cert), inside the 0700 caDir.
// Atomic via write-then-rename so a crashed write doesn't leave a
// partially-formed file the next start would try to parse.
func generateAndPersistCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	key, certPEM, cert, err := newSelfSignedCA()
	if err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writeAtomic(filepath.Join(dir, caCertFilename), certPEM, 0o644); err != nil {
		return nil, nil, nil, err
	}
	if err := writeAtomic(filepath.Join(dir, caKeyFilename), keyPEM, 0o600); err != nil {
		return nil, nil, nil, err
	}
	return cert, key, certPEM, nil
}

// pathExists checks whether a path is present. Distinguishes "file not
// found" (returns false, nil) from real errors (returns false, err) so
// transient stat failures don't get silently treated as a missing
// file (which would trigger CA regeneration over a working chain).
func pathExists(p string) (bool, error) {
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// writeAtomic writes data to a temp file in the same dir and renames
// to path. Avoids leaving a partially-written file if the process
// crashes mid-write — important for the CA key, which a partial
// state would render unparseable for the next daemon start.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %q: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op when rename succeeds
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp for %q: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp for %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %q: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %q → %q: %w", tmpName, path, err)
	}
	return nil
}

// newSelfSignedCA returns (caKey, caCertPEM, caCert, error). The
// returned key + parsed cert are kept by the caller so we can sign
// other certs (i.e. the server cert) without re-decoding the PEM.
func newSelfSignedCA() (*ecdsa.PrivateKey, []byte, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "fj-bellows cache CA",
			Organization: []string{"fj-bellows"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sign CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return key, pemBytes, cert, nil
}

// newServerCert mints a leaf cert signed by caCert/caKey. The cert
// includes a DNS SAN matching cacheHost so workers can verify by
// hostname; a localhost SAN + 127.0.0.1 IP SAN cover operator break-
// glass via SSH tunnel.
func newServerCert(cacheHost string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate server key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cacheHost},
		DNSNames:     []string{cacheHost, "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("sign server cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal server key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// randomSerial returns a 128-bit positive serial number. Required by
// RFC 5280; collisions with 128 bits of entropy are not a concern.
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return n, nil
}
