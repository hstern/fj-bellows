package linode

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestUpstreamPort(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{in: "https://host/v2/", want: 443},
		{in: "http://host/v2/", want: 80},
		{in: "https://host:8443/v2/", want: 8443},
		{in: "http://host:5000/v2/", want: 5000},
		{in: "ftp://host/v2/", want: 0},
		{in: "https://host:0/", want: 0},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			u, err := url.Parse(c.in)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := upstreamPort(u); got != c.want {
				t.Errorf("upstreamPort(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestCacheTunnelTOFUHostKey(t *testing.T) {
	keyA := newTestPubKey(t)
	keyB := newTestPubKey(t)

	tn := &cacheTunnel{}
	const addr1 = "10.0.0.1:22"

	cb := tn.tofuHostKeyCallback(addr1)
	if err := cb("", nil, keyA); err != nil {
		t.Fatalf("first contact should pin and accept: %v", err)
	}
	if err := cb("", nil, keyA); err != nil {
		t.Fatalf("matching key should keep accepting: %v", err)
	}
	if err := cb("", nil, keyB); err == nil {
		t.Fatal("mismatched key on pinned addr must be rejected (cache-VM MITM)")
	}

	// Different addrs are independent.
	cb2 := tn.tofuHostKeyCallback("10.0.0.2:22")
	if err := cb2("", nil, keyB); err != nil {
		t.Fatalf("distinct addr should pin its own key: %v", err)
	}
}

// TestCacheTunnelStopBeforeStart verifies the lifecycle is safe to
// call in any order — Stop on an un-Started tunnel must not panic or
// block (matters because the managedCache cleanup path may run after
// a Configure that bailed before startTunnel).
func TestCacheTunnelStopBeforeStart(_ *testing.T) {
	tn := &cacheTunnel{}
	tn.Stop() // must not panic, must not block
}

// TestCacheTunnelStopAfterStart asserts the loop exits within a small
// timeout when Stop is called, even while it's mid-backoff sleep.
func TestCacheTunnelStopAfterStart(t *testing.T) {
	tn := &cacheTunnel{
		signer:         newTestSigner(t),
		sshUser:        "root",
		sshPort:        22,
		upstreamHost:   "u",
		upstreamPort:   443,
		initialBackoff: 50 * time.Millisecond,
		maxBackoff:     50 * time.Millisecond,
		dialTimeout:    10 * time.Millisecond,
		lookupIP: func(context.Context) (string, error) {
			return "", errors.New("no ip yet")
		},
		log: slog.Default(),
	}
	tn.Start()
	time.Sleep(150 * time.Millisecond)
	stopDone := make(chan struct{})
	go func() {
		tn.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s — loop is leaking")
	}
}

// TestCacheTunnelLoopRetriesOnLookupError exercises the reconnect
// backoff: lookupIP errors should NOT spin (busy-loop) but instead
// honor initialBackoff. We count attempts in a fixed window and
// assert the rate matches the configured backoff curve, not
// microsecond retry storms.
func TestCacheTunnelLoopRetriesOnLookupError(t *testing.T) {
	var attempts atomic.Int32
	tn := &cacheTunnel{
		signer:         newTestSigner(t),
		sshUser:        "root",
		sshPort:        22,
		upstreamHost:   "u",
		upstreamPort:   443,
		initialBackoff: 30 * time.Millisecond,
		maxBackoff:     30 * time.Millisecond,
		dialTimeout:    5 * time.Millisecond,
		lookupIP: func(context.Context) (string, error) {
			attempts.Add(1)
			return "", errors.New("ip not assigned")
		},
		log: slog.Default(),
	}
	tn.Start()
	defer tn.Stop()

	// Over ~200ms with a 30ms backoff, expect a few attempts. Use a
	// loose upper bound — the goal is to rule out busy-spinning
	// (which would yield thousands).
	time.Sleep(200 * time.Millisecond)
	n := attempts.Load()
	if n < 2 {
		t.Errorf("expected at least 2 attempts in 200ms with 30ms backoff, got %d", n)
	}
	if n > 50 {
		t.Errorf("expected ≤50 attempts in 200ms with 30ms backoff (busy-loop?), got %d", n)
	}
}

// TestCacheTunnelForwardBridgesBytes verifies the per-conn forward
// reads from the (cache-side) conn and writes to the upstream dial,
// and vice-versa, until either side closes. The bridge is the load-
// bearing part for FJB-7 — without it, accepted SSH reverse-forward
// connections would float orphan.
func TestCacheTunnelForwardBridgesBytes(t *testing.T) {
	cacheA, cacheB := net.Pipe()
	upstreamA, upstreamB := net.Pipe()

	tn := &cacheTunnel{
		upstreamHost: "ignored-by-stub-dialer",
		upstreamPort: 443,
		dialUpstream: func(context.Context, string, string) (net.Conn, error) {
			return upstreamA, nil
		},
		log: slog.Default(),
	}

	done := make(chan struct{})
	go func() {
		tn.forward(context.Background(), cacheB)
		close(done)
	}()

	// cache → upstream
	cacheWrite := []byte("from-cache-to-upstream")
	go func() { _, _ = cacheA.Write(cacheWrite) }()
	gotUp := readN(t, upstreamB, len(cacheWrite))
	if string(gotUp) != string(cacheWrite) {
		t.Errorf("upstream got %q, want %q", gotUp, cacheWrite)
	}

	// upstream → cache
	upWrite := []byte("from-upstream-to-cache")
	go func() { _, _ = upstreamB.Write(upWrite) }()
	gotDown := readN(t, cacheA, len(upWrite))
	if string(gotDown) != string(upWrite) {
		t.Errorf("cache got %q, want %q", gotDown, upWrite)
	}

	// Close both ends — both io.Copy loops in forward return EOF and
	// the function exits.
	_ = cacheA.Close()
	_ = upstreamB.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("forward did not return after both sides closed")
	}
}

// TestCacheTunnelForwardDropsOnUpstreamDialError covers the "log and
// move on" semantics when upstream is unreachable: forward must close
// the accepted conn (not leak it) and return.
func TestCacheTunnelForwardDropsOnUpstreamDialError(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	tn := &cacheTunnel{
		upstreamHost: "u",
		upstreamPort: 443,
		dialUpstream: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("simulated dial failure")
		},
		log: slog.Default(),
	}
	done := make(chan struct{})
	go func() {
		tn.forward(context.Background(), b)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forward did not return after dial failure")
	}
}

// TestStartTunnelSkipsWhenSignerNil covers the managedCache wiring
// guard: tests against the fake cache client (no SSH) must not start
// a tunnel that would then leak a goroutine.
func TestStartTunnelSkipsWhenSignerNil(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(t, fc, bucket)
	cache.setHardwareContext(7777, 8888, "")
	// signer left nil
	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	if cache.tunnel != nil {
		t.Fatalf("tunnel must not start when signer is nil; got %+v", cache.tunnel)
	}
}

// TestStartTunnelKicksWhenSignerSet asserts the tunnel goroutine is
// created when setTunnelIdentity supplies a signer. The fake cache
// client returns no IPv4 from GetInstance, so the loop stays in its
// lookup-retry phase — no real SSH dial is attempted, and Stop drains
// cleanly via maybeCleanupCache.
func TestStartTunnelKicksWhenSignerSet(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(t, fc, bucket)
	cache.setHardwareContext(7777, 8888, "ssh-ed25519 AAA test")
	cache.setTunnelIdentity(newTestSigner(t), "root", 22)
	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	if cache.tunnel == nil {
		t.Fatal("expected tunnel to start when signer is set")
	}
	cache.maybeCleanupCache(ctx)
	if cache.tunnel != nil {
		t.Errorf("tunnel should be nilled by maybeCleanupCache")
	}
}

// TestRenderCacheCloudInitOmitsHostsOverrideForIPLiteral guards the
// template's `if not isIPLiteral` branch — when the upstream URL uses
// a bare IP, /etc/hosts can't redirect (it maps names not IPs), and
// the cloud-init must skip the override line cleanly rather than
// emit garbage.
func TestRenderCacheCloudInitOmitsHostsOverrideForIPLiteral(t *testing.T) {
	out, err := renderCacheCloudInit(cacheCloudInitParams{
		Bucket: "b", Region: "r", Endpoint: testStubEndpoint,
		AccessKey: "AK", SecretKey: "SK", ZotVersion: testStubZotVersion,
		ServerCertPEM: testStubPEM, ServerKeyPEM: testStubPEM,
		UpstreamURL: "https://10.0.0.5/v2/", UpstreamHost: "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out, "127.0.0.1 10.0.0.5") {
		t.Errorf("rendered cloud-init must not /etc/hosts-override an IP literal:\n%s", out)
	}
}

// TestRenderCacheCloudInitEmitsHostsOverrideForHostname is the other
// half — for a hostname upstream the template MUST emit the override
// block so zot's sync extension hits the orchestrator's reverse-
// tunnel listener on 127.0.0.1.
func TestRenderCacheCloudInitEmitsHostsOverrideForHostname(t *testing.T) {
	out, err := renderCacheCloudInit(cacheCloudInitParams{
		Bucket: "b", Region: "r", Endpoint: testStubEndpoint,
		AccessKey: "AK", SecretKey: "SK", ZotVersion: testStubZotVersion,
		ServerCertPEM: testStubPEM, ServerKeyPEM: testStubPEM,
		UpstreamURL: "https://git.example/v2/", UpstreamHost: "git.example",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"127.0.0.1 git.example",
		"sshd_config.d/20-fj-bellows-tunnel.conf",
		"AllowTcpForwarding yes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered cloud-init missing %q:\n%s", want, out)
		}
	}
}

// --- helpers ---

func newTestPubKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519: %v", err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("NewPublicKey: %v", err)
	}
	return pk
}

func newTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519: %v", err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	return s
}

// readN reads exactly n bytes from r with a deadline, returning what
// was read. Fails the test if the deadline expires before n bytes
// arrive.
func readN(t *testing.T, r net.Conn, n int) []byte {
	t.Helper()
	_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, n)
	off := 0
	for off < n {
		k, err := r.Read(buf[off:])
		if err != nil {
			t.Fatalf("Read: %v (got %d/%d bytes)", err, off, n)
		}
		off += k
	}
	return buf
}
