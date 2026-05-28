package wg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/transport/wg/acl"
)

// startUpstream spins up a loopback TCP echo server. The test closes
// it via the returned closer.
func startUpstream(t *testing.T, handle func(net.Conn)) (addr string, closer func()) {
	t.Helper()
	lc := net.ListenConfig{}
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				handle(c)
			}(c)
		}
	})
	return ln.Addr().String(), func() {
		_ = ln.Close()
		wg.Wait()
	}
}

// echoHandler reads all bytes and writes them back, then closes.
func echoHandler(c net.Conn) {
	_, _ = io.Copy(c, c)
}

// fakeRegistry is a minimal acl.Registry stand-in for the proxy tests
// — it owns an acl.Snapshot (built from operator-style ACL strings)
// and a notification channel callers can pulse to simulate
// registry changes. The real Registry's Snapshot/Subscribe contract
// is identical; we use a fake so the tests don't depend on the
// resolver loop or DNS plumbing.
type fakeRegistry struct {
	mu      sync.Mutex
	current acl.Snapshot
	subs    []chan struct{}
}

func newFakeRegistry(t *testing.T, raw ...string) *fakeRegistry {
	t.Helper()
	entries, err := acl.Parse(raw)
	if err != nil {
		t.Fatalf("acl.Parse: %v", err)
	}
	r := &fakeRegistry{}
	r.current = buildSnapshot(entries, nil)
	return r
}

// setDomainAddrs swaps in a snapshot where the named domain entries
// have the supplied addresses. Tests use this to simulate the
// resolver loop pushing a new address set.
func (r *fakeRegistry) setDomainAddrs(t *testing.T, raw []string, resolved map[string][]netip.Addr) {
	t.Helper()
	entries, err := acl.Parse(raw)
	if err != nil {
		t.Fatalf("acl.Parse: %v", err)
	}
	r.mu.Lock()
	r.current = buildSnapshot(entries, resolved)
	subs := append([]chan struct{}(nil), r.subs...)
	r.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Snapshot satisfies the Registry interface.
func (r *fakeRegistry) Snapshot() acl.Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// Subscribe satisfies the Registry interface.
func (r *fakeRegistry) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	r.subs = append(r.subs, ch)
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, c := range r.subs {
			if c == ch {
				r.subs = append(r.subs[:i], r.subs[i+1:]...)
				close(c)
				return
			}
		}
	}
}

// buildSnapshot returns an acl.Snapshot equivalent to what
// NewRegistry would produce after running setResolved for every
// entry in resolved. Uses the public acl.NewRegistry constructor +
// reflection-free path so the test never reaches into Snapshot's
// unexported map.
func buildSnapshot(entries []acl.Entry, resolved map[string][]netip.Addr) acl.Snapshot {
	reg := acl.NewRegistry(entries)
	for host, addrs := range resolved {
		// setResolved is unexported; we rebuild via NewRegistry +
		// an exported "set domain" path. The registry exposes
		// only Snapshot + Subscribe publicly — the resolver loop
		// hits setResolved through the same struct. To stay
		// public-API-only in the test, we re-issue Subscribe
		// then drive the registry by going through ProcessLookup
		// (acl.LookupResult), which the resolver uses internally
		// via runHost. Direct route: invoke the resolver loop's
		// single-pass path via a synchronous helper.
		_ = host
		_ = addrs
	}
	// The acl.Registry public surface doesn't expose a "set
	// domain addrs" method — only Snapshot/Subscribe. For the
	// purposes of these tests we fall back to constructing a
	// Snapshot via the Registry's normal path: feed a resolver
	// that returns the desired addresses synchronously.
	if len(resolved) == 0 {
		return reg.Snapshot()
	}
	res := &fakeLookup{results: make(map[string]acl.LookupResult)}
	for host, addrs := range resolved {
		res.results[host] = acl.LookupResult{Addrs: addrs, TTL: time.Hour}
	}
	resolver := acl.NewResolver(reg, res)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = resolver.Start(ctx)
	// Give the resolver goroutines a moment to push their first
	// result before snapping. The Subscribe channel offers a
	// deterministic wait point.
	ch, unsub := reg.Subscribe()
	defer unsub()
	expectChanges := len(resolved)
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for got := 0; got < expectChanges; {
		select {
		case <-ch:
			got++
		case <-deadline.C:
			cancel()
			resolver.Close()
			return reg.Snapshot()
		}
	}
	cancel()
	resolver.Close()
	return reg.Snapshot()
}

// fakeLookup is a one-shot DNS stub used by buildSnapshot.
type fakeLookup struct {
	results map[string]acl.LookupResult
}

func (f *fakeLookup) LookupHost(_ context.Context, host string) (acl.LookupResult, error) {
	if r, ok := f.results[host]; ok {
		return r, nil
	}
	return acl.LookupResult{}, fmt.Errorf("no fake result for %q", host)
}

// loopbackTunnel implements the Listener interface by binding plain
// loopback TCP listeners. The proxy never touches the netstack types
// — only what's behind ListenTCP — so loopback is interchangeable
// with the real wg netstack for accept/dispatch tests.
type loopbackTunnel struct {
	t     *testing.T
	mu    sync.Mutex
	addrs map[netip.AddrPort]string // bound netip.AddrPort -> loopback addr string
}

func newLoopbackTunnel(t *testing.T) *loopbackTunnel {
	t.Helper()
	return &loopbackTunnel{t: t, addrs: make(map[netip.AddrPort]string)}
}

// ListenTCP allocates a loopback listener and records the mapping
// from the requested (addr, port) to the actual loopback address
// the test client should dial.
func (lt *loopbackTunnel) ListenTCP(addr *net.TCPAddr) (net.Listener, error) {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(lt.t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ip, _ := netip.AddrFromSlice(addr.IP)
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	key := netip.AddrPortFrom(ip, portToUint16(addr.Port))
	lt.mu.Lock()
	lt.addrs[key] = ln.Addr().String()
	lt.mu.Unlock()
	// Wrap the listener so accepted conns advertise the requested
	// (addr, port) on LocalAddr — the proxy's ACL dispatch keys on
	// that value, not on the loopback's 127.0.0.1:N.
	return &remappedListener{Listener: ln, local: &net.TCPAddr{IP: addr.IP, Port: addr.Port}}, nil
}

// loopbackAddrFor returns the loopback address tests should dial to
// reach a given (ACL-level) (addr, port).
func (lt *loopbackTunnel) loopbackAddrFor(addrPort netip.AddrPort) (string, bool) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	s, ok := lt.addrs[addrPort]
	return s, ok
}

// remappedListener overrides Accept to wrap returned conns in
// remappedConn so LocalAddr matches the netstack-level (addr, port)
// the proxy registered for, not the loopback's actual bind.
type remappedListener struct {
	net.Listener
	local *net.TCPAddr
}

func (r *remappedListener) Accept() (net.Conn, error) {
	c, err := r.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &remappedConn{Conn: c, local: r.local}, nil
}

// remappedConn forwards LocalAddr to the netstack-level address. All
// other methods delegate to the underlying loopback Conn.
type remappedConn struct {
	net.Conn
	local *net.TCPAddr
}

func (r *remappedConn) LocalAddr() net.Addr { return r.local }

// CloseWrite delegates to the underlying TCPConn so half-close still
// works for the half-close test.
func (r *remappedConn) CloseWrite() error {
	if tc, ok := r.Conn.(*net.TCPConn); ok {
		return tc.CloseWrite()
	}
	return nil
}

// startProxy wires up the proxy with a loopback tunnel and returns
// the proxy + tunnel for the test to drive. Cleanup torn down by
// t.Cleanup.
func startProxy(t *testing.T, reg Registry, opts ...Option) (*Proxy, *loopbackTunnel) {
	t.Helper()
	lt := newLoopbackTunnel(t)
	p, err := NewProxy(lt, reg, slog.New(slog.DiscardHandler), opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, lt
}

func dialLoopback(t *testing.T, addr string) net.Conn {
	t.Helper()
	d := net.Dialer{}
	c, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// waitForListener polls the loopback tunnel until it has registered
// a mapping for ap, or the test deadline fires. Reconcile is
// synchronous on Start but async on snapshot updates; the watch loop
// may not have fired yet.
func waitForListener(t *testing.T, lt *loopbackTunnel, ap netip.AddrPort) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if a, ok := lt.loopbackAddrFor(ap); ok {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("listener for %s never registered", ap)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProxy_BridgesByteStream_LiteralEntry(t *testing.T) {
	upstreamAddr, closeUpstream := startUpstream(t, echoHandler)
	defer closeUpstream()

	// The test upstream lives at upstreamAddr (127.0.0.1:NNN). We
	// pin the ACL to that same literal so the proxy dials back to
	// it on accept.
	upHost, upPort, _ := net.SplitHostPort(upstreamAddr)
	reg := newFakeRegistry(t, fmt.Sprintf("tcp://%s:%s", upHost, upPort))

	_, lt := startProxy(t, reg)

	ap := netip.MustParseAddrPort(upstreamAddr)
	loopback := waitForListener(t, lt, ap)
	c := dialLoopback(t, loopback)
	defer func() { _ = c.Close() }()

	payload := []byte("hello-from-fjb-wg-proxy")
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("echo mismatch: got %q want %q", got, payload)
	}
}

func TestProxy_BridgesByteStream_DomainEntry(t *testing.T) {
	upstreamAddr, closeUpstream := startUpstream(t, echoHandler)
	defer closeUpstream()

	// Domain entry: workers dial "echo.test:PORT"; the resolver loop
	// pins it to a specific address (here, the upstream's loopback
	// address so the netstack listens on the same /32). The proxy's
	// host-side dialer is intercepted to send the dial back to the
	// upstream (simulating the host resolver agreeing).
	upHost, upPort, _ := net.SplitHostPort(upstreamAddr)
	pinned := netip.MustParseAddr(upHost)
	port := upPort

	reg := newFakeRegistry(t)
	reg.setDomainAddrs(t,
		[]string{"tcp://echo.test:" + port},
		map[string][]netip.Addr{"echo.test": {pinned}},
	)

	dialCalls := make(chan string, 1)
	intercept := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialCalls <- address
		// The upstream is a literal 127.0.0.1:N — short-circuit
		// "echo.test:N" to the real address.
		return (&net.Dialer{}).DialContext(ctx, network, upstreamAddr)
	}

	_, lt := startProxy(t, reg, withDialContext(intercept))

	ap := netip.AddrPortFrom(pinned, portToUint16(mustAtoi(t, port)))
	loopback := waitForListener(t, lt, ap)
	c := dialLoopback(t, loopback)
	defer func() { _ = c.Close() }()

	payload := []byte("domain-entry-bridge")
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("echo mismatch: got %q want %q", got, payload)
	}
	select {
	case dialed := <-dialCalls:
		// Domain entries dial the entry host (so the host resolver
		// picks the current authoritative IP), not the netstack
		// address — verify the proxy honoured that contract.
		want := "echo.test:" + port
		if dialed != want {
			t.Errorf("upstream dial = %q, want %q", dialed, want)
		}
	default:
		t.Error("dialer was not called")
	}
}

// Port-spec enforcement: an entry with ports {443} should NOT cause
// the proxy to listen on port 22 or accept a connection routed to
// it. With the wildcard model we only ever open listeners for ports
// in PortSpec, so the test confirms by the absence of a listener.
func TestProxy_PortSpecOnlyListensOnDeclaredPorts(t *testing.T) {
	reg := newFakeRegistry(t, "tcp://192.0.2.1:443")
	_, lt := startProxy(t, reg)

	// Wait for the declared listener to come up.
	declared := netip.MustParseAddrPort("192.0.2.1:443")
	_ = waitForListener(t, lt, declared)

	// A port not in the spec must not have a listener.
	if _, ok := lt.loopbackAddrFor(netip.MustParseAddrPort("192.0.2.1:22")); ok {
		t.Error("listener for undeclared port 22 should not exist")
	}
}

// Snapshot change → reconcile picks up the new (addr, port) and
// opens a listener for it.
func TestProxy_SnapshotChangeOpensNewListener(t *testing.T) {
	upstreamAddr, closeUpstream := startUpstream(t, echoHandler)
	defer closeUpstream()
	upHost, upPort, _ := net.SplitHostPort(upstreamAddr)
	pinned := netip.MustParseAddr(upHost)

	reg := newFakeRegistry(t) // no entries yet
	_, lt := startProxy(t, reg)

	// Push a snapshot with a single domain entry → resolver-driven
	// /32 → new listener.
	reg.setDomainAddrs(t,
		[]string{"tcp://echo.test:" + upPort},
		map[string][]netip.Addr{"echo.test": {pinned}},
	)

	ap := netip.AddrPortFrom(pinned, portToUint16(mustAtoi(t, upPort)))
	_ = waitForListener(t, lt, ap)
}

// In-flight connection survives a snapshot change. A removed
// listener stops accepting but the already-running bridge continues
// until the client/upstream close.
func TestProxy_InFlightConnSurvivesSnapshotChange(t *testing.T) {
	// Use a manual upstream that doesn't echo until told — keeps
	// the bridge open while we mutate the snapshot.
	type pair struct {
		client net.Conn
		closer chan struct{}
	}
	pairs := make(chan pair, 1)
	lc := net.ListenConfig{}
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			done := make(chan struct{})
			pairs <- pair{client: c, closer: done}
			go func() {
				<-done
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}()
		}
	})
	defer func() {
		_ = ln.Close()
		wg.Wait()
	}()

	upstreamAddr := ln.Addr().String()
	upHost, upPort, _ := net.SplitHostPort(upstreamAddr)
	reg := newFakeRegistry(t, fmt.Sprintf("tcp://%s:%s", upHost, upPort))
	_, lt := startProxy(t, reg)

	ap := netip.MustParseAddrPort(upstreamAddr)
	loopback := waitForListener(t, lt, ap)
	c := dialLoopback(t, loopback)
	defer func() { _ = c.Close() }()

	// Make sure the bridge is fully established before mutating.
	if _, err := c.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	gotPair := <-pairs

	// Mutate snapshot so the listener for ap goes away.
	reg.setDomainAddrs(t, nil, nil)

	// Listener should eventually disappear from the tunnel's
	// registry. We don't have a direct "is closed" probe; instead
	// confirm the in-flight bridge still works.
	if _, err := c.Write([]byte(" + tail")); err != nil {
		t.Fatalf("in-flight write after snapshot change failed: %v", err)
	}
	// Unblock the upstream so its drain goroutine exits cleanly.
	close(gotPair.closer)
}

// Refused: an accepted connection whose (LocalAddr, port) doesn't
// match any ACL entry is closed without dialing upstream. We trigger
// this by binding a stale listener (snapshot change drops the
// entry), then dialing the still-open loopback.
//
// We can also exercise it directly: open a listener manually by
// calling tun.ListenTCP for a key the proxy never registered, then
// drive a connection through and confirm no upstream was dialed.
// The simpler path uses port-spec mismatch — but the proxy only
// opens listeners for valid pairs, so refused-by-Lookup requires a
// stale listener. Achieve that by mutating the snapshot mid-flight.
func TestProxy_RefusesUnmatchedAfterSnapshotChange(t *testing.T) {
	// Upstream that fails fast if dialed.
	upstreamAddr, closeUpstream := startUpstream(t, echoHandler)
	defer closeUpstream()
	upHost, upPort, _ := net.SplitHostPort(upstreamAddr)
	reg := newFakeRegistry(t, fmt.Sprintf("tcp://%s:%s", upHost, upPort))

	// Track upstream dials so we can assert none happened.
	var dialCount int
	var mu sync.Mutex
	intercept := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dialCount++
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}

	_, lt := startProxy(t, reg, withDialContext(intercept))

	ap := netip.MustParseAddrPort(upstreamAddr)
	loopback := waitForListener(t, lt, ap)

	// Now wipe the ACL — listener should close. Race against the
	// reconcile loop by dialing in a tight retry: if reconcile
	// hasn't yet closed the listener and Lookup fails, the proxy
	// rejects the conn without dialing upstream.
	reg.setDomainAddrs(t, nil, nil)

	// Best-effort: try dialing the (now stale) loopback. If the
	// listener already closed the dial errors, which is also a
	// correct outcome. If it's still up briefly, Accept fires +
	// Lookup returns nil + the conn is closed without an upstream
	// dial.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		dctx, dcancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		c, err := (&net.Dialer{}).DialContext(dctx, "tcp", loopback)
		dcancel()
		if err != nil {
			// Listener gone — correct.
			break
		}
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _ = c.Read(make([]byte, 1))
		_ = c.Close()
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if dialCount != 0 {
		t.Errorf("upstream dialed %d times after ACL wipe; want 0", dialCount)
	}
}

// Close drains: connection in-flight, Close returns within a short
// deadline, second Close is a no-op.
func TestProxy_CloseIdempotentAndDrains(t *testing.T) {
	upstreamAddr, closeUpstream := startUpstream(t, echoHandler)
	defer closeUpstream()
	upHost, upPort, _ := net.SplitHostPort(upstreamAddr)
	reg := newFakeRegistry(t, fmt.Sprintf("tcp://%s:%s", upHost, upPort))

	lt := newLoopbackTunnel(t)
	p, err := NewProxy(lt, reg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}

	ap := netip.MustParseAddrPort(upstreamAddr)
	loopback := waitForListener(t, lt, ap)
	c := dialLoopback(t, loopback)
	defer func() { _ = c.Close() }()

	done := make(chan error, 1)
	go func() { done <- p.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung; bridges were not force-closed")
	}

	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// Post-Close dials are rejected because the listener is closed.
	d := net.Dialer{}
	dialCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := d.DialContext(dialCtx, "tcp", loopback); err == nil {
		t.Error("dial after Close should error (listener closed)")
	} else if !errors.Is(err, errConnRefused()) {
		t.Logf("post-Close dial error (informational): %v", err)
	}
}

func TestProxy_RejectsNilArgs(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if _, err := NewProxy(nil, &fakeRegistry{}, log); err == nil {
		t.Error("nil tunnel should error")
	}
	if _, err := NewProxy(newLoopbackTunnel(t), nil, log); err == nil {
		t.Error("nil registry should error")
	}
}

// errConnRefused returns a sentinel for the platform-specific
// connection-refused error.
func errConnRefused() error { return errors.New("connection refused") }

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}
