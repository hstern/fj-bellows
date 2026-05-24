package linode

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrGenerateCertPairFreshDir(t *testing.T) {
	dir := t.TempDir()
	pair, fresh, err := loadOrGenerateCertPair(dir, defaultCacheHostname)
	if err != nil {
		t.Fatalf("loadOrGenerateCertPair: %v", err)
	}
	if !fresh {
		t.Errorf("expected freshCA=true on empty dir")
	}
	if len(pair.CACertPEM) == 0 || len(pair.ServerCertPEM) == 0 || len(pair.ServerKeyPEM) == 0 {
		t.Fatalf("pair has empty fields: %+v", pair)
	}
	// CA pem files were persisted.
	for _, name := range []string{caCertFilename, caKeyFilename} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s not persisted: %v", name, err)
		}
	}
	// Server cert verifies against the persisted CA — the chain works.
	assertServerCertSignedByCA(t, pair.ServerCertPEM, pair.CACertPEM, defaultCacheHostname)
}

func TestLoadOrGenerateCertPairReusesCAOnSecondCall(t *testing.T) {
	dir := t.TempDir()
	first, fresh1, err := loadOrGenerateCertPair(dir, defaultCacheHostname)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if !fresh1 {
		t.Errorf("first call should be fresh")
	}
	second, fresh2, err := loadOrGenerateCertPair(dir, defaultCacheHostname)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if fresh2 {
		t.Errorf("second call should report freshCA=false (persistent CA)")
	}
	if string(first.CACertPEM) != string(second.CACertPEM) {
		t.Errorf("CA cert changed between calls — persistence not working")
	}
	// Server cert IS expected to differ (we sign a new one each call)
	// — the cache VM gets a fresh cert on each Provision while
	// workers trust the same CA across daemon restarts.
	if string(first.ServerCertPEM) == string(second.ServerCertPEM) {
		t.Errorf("server cert should be regenerated per call")
	}
}

func TestLoadCAReportsPartialState(t *testing.T) {
	dir := t.TempDir()
	// Seed only the cert; leave the key missing.
	if err := os.WriteFile(filepath.Join(dir, caCertFilename), []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := loadCA(dir)
	if err == nil {
		t.Fatal("expected error on partial CA state (cert present, key missing)")
	}
	if !strings.Contains(err.Error(), "partial") {
		t.Errorf("error should mention partial state, got: %v", err)
	}
}

func TestLoadCARejectsCorruptedPEM(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, caCertFilename), []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, caKeyFilename), []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := loadCA(dir)
	if err == nil {
		t.Fatal("expected error on corrupted PEM")
	}
}

func TestWriteAtomicProducesNoLeftoverOnSuccess(t *testing.T) {
	dir := t.TempDir()
	if err := writeAtomic(filepath.Join(dir, "test"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "test.tmp-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestLoadOrGenerateCertPairValidatesArgs(t *testing.T) {
	if _, _, err := loadOrGenerateCertPair("/tmp", ""); err == nil {
		t.Error("empty cacheHost should error")
	}
	if _, _, err := loadOrGenerateCertPair("", defaultCacheHostname); err == nil {
		t.Error("empty caDir should error")
	}
}

// assertServerCertSignedByCA verifies the chain: parse the server cert
// from PEM, parse the CA cert, build a CertPool with just the CA, and
// run a Verify with the cache hostname as DNS name. This is what
// workers do via /etc/ssl/certs and openssl — we mimic it here.
func assertServerCertSignedByCA(t *testing.T, serverCertPEM, caCertPEM []byte, host string) {
	t.Helper()
	serverBlock, _ := pem.Decode(serverCertPEM)
	if serverBlock == nil {
		t.Fatal("server cert: not PEM")
	}
	server, err := x509.ParseCertificate(serverBlock.Bytes)
	if err != nil {
		t.Fatalf("server cert parse: %v", err)
	}
	caBlock, _ := pem.Decode(caCertPEM)
	if caBlock == nil {
		t.Fatal("CA cert: not PEM")
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("CA cert parse: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	opts := x509.VerifyOptions{Roots: pool, DNSName: host}
	if _, err := server.Verify(opts); err != nil {
		t.Errorf("server cert does not verify against CA for %q: %v", host, err)
	}
}
