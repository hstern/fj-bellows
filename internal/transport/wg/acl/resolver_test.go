package acl

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// fakeLookup is a fully controllable Lookup implementation backed by a
// per-host queue of programmed responses. Tests push responses onto
// the queue; the resolver dequeues them in order.
type fakeLookup struct {
	mu       sync.Mutex
	queues   map[string][]fakeResponse
	notify   map[string]chan struct{}
	callCh   chan string
	defaults map[string]fakeResponse
}

type fakeResponse struct {
	addrs []netip.Addr
	ttl   time.Duration
	err   error
}

func newFakeLookup() *fakeLookup {
	return &fakeLookup{
		queues:   make(map[string][]fakeResponse),
		notify:   make(map[string]chan struct{}),
		callCh:   make(chan string, 32),
		defaults: make(map[string]fakeResponse),
	}
}

func (f *fakeLookup) push(host string, addrs []netip.Addr, ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues[host] = append(f.queues[host], fakeResponse{addrs: addrs, ttl: ttl})
	if ch := f.notify[host]; ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (f *fakeLookup) pushErr(host string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues[host] = append(f.queues[host], fakeResponse{err: err})
	if ch := f.notify[host]; ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (f *fakeLookup) setDefault(host string, addrs []netip.Addr, ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaults[host] = fakeResponse{addrs: addrs, ttl: ttl}
}

func (f *fakeLookup) LookupHost(_ context.Context, host string) (LookupResult, error) {
	f.mu.Lock()
	q := f.queues[host]
	var r fakeResponse
	if len(q) > 0 {
		r = q[0]
		f.queues[host] = q[1:]
	} else if d, ok := f.defaults[host]; ok {
		r = d
	} else {
		r = fakeResponse{err: errors.New("fakeLookup: nothing queued for " + host)}
	}
	f.mu.Unlock()

	select {
	case f.callCh <- host:
	default:
	}
	if r.err != nil {
		return LookupResult{}, r.err
	}
	return LookupResult{Addrs: r.addrs, TTL: r.ttl}, nil
}

// fakeClock lets tests drive the resolver's sleep loop deterministically.
// After-channel sends are gated on advance().
type fakeClock struct {
	mu      sync.Mutex
	waiters []fakeWaiter
}

type fakeWaiter struct {
	d  time.Duration
	ch chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{} }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, fakeWaiter{d: d, ch: ch})
	return ch
}

// advanceAll wakes every pending waiter, regardless of declared
// duration — sufficient for tests because the resolver loop only ever
// has one waiter per goroutine outstanding.
func (c *fakeClock) advanceAll() {
	c.mu.Lock()
	w := c.waiters
	c.waiters = nil
	c.mu.Unlock()
	for _, x := range w {
		select {
		case x.ch <- time.Now():
		default:
		}
	}
}

// waitForWaiters blocks until at least n waiters are registered (used
// to synchronise tests with the resolver loop's "about to sleep" state).
func (c *fakeClock) waitForWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := len(c.waiters)
		c.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited 2s for %d waiters", n)
}

func withClock(c *fakeClock) ResolverOption {
	return func(r *Resolver) { r.clock = c }
}

// TestResolver_InitialResolution verifies a domain entry's first
// lookup writes addrs into the registry snapshot.
func TestResolver_InitialResolution(t *testing.T) {
	entries := mustParse(t, []string{tcpDomain})
	reg := NewRegistry(entries)

	fake := newFakeLookup()
	addr := netip.MustParseAddr("1.2.3.4")
	fake.push(exampleHost, []netip.Addr{addr}, 5*time.Minute)
	fake.setDefault(exampleHost, []netip.Addr{addr}, 5*time.Minute)

	clk := newFakeClock()
	res := NewResolver(reg, fake, withClock(clk))
	if err := res.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(res.Close)

	// Wait for the first lookup + push into registry.
	waitForSnapshot(t, reg, func(s Snapshot) bool {
		return len(s.Prefixes) == 1 && s.Prefixes[0].Addr() == addr
	})
}

// TestResolver_RefreshOnTTL verifies the resolver re-resolves after
// the per-host TTL elapses and that an address-set change wakes
// subscribers.
func TestResolver_RefreshOnTTL(t *testing.T) {
	entries := mustParse(t, []string{tcpDomain})
	reg := NewRegistry(entries)
	sub, unsub := reg.Subscribe()
	defer unsub()

	fake := newFakeLookup()
	a1 := netip.MustParseAddr("1.2.3.4")
	a2 := netip.MustParseAddr("5.6.7.8")
	fake.push(exampleHost, []netip.Addr{a1}, 60*time.Second)
	fake.push(exampleHost, []netip.Addr{a2}, 60*time.Second)
	fake.setDefault(exampleHost, []netip.Addr{a2}, 60*time.Second)

	clk := newFakeClock()
	res := NewResolver(reg, fake, withClock(clk))
	if err := res.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(res.Close)

	// Drain the initial-resolution notification.
	waitForNotify(t, sub)
	if got := reg.Snapshot().Prefixes[0].Addr(); got != a1 {
		t.Fatalf("initial addr = %v, want %v", got, a1)
	}

	// Loop is now in After(60s). Trigger refresh.
	clk.waitForWaiters(t, 1)
	clk.advanceAll()

	waitForNotify(t, sub)
	if got := reg.Snapshot().Prefixes[0].Addr(); got != a2 {
		t.Fatalf("post-refresh addr = %v, want %v", got, a2)
	}
}

// TestResolver_MixedAAAA verifies A and AAAA addrs in the same answer
// both land in the snapshot as /32 and /128 prefixes respectively.
func TestResolver_MixedAAAA(t *testing.T) {
	entries := mustParse(t, []string{tcpDomain})
	reg := NewRegistry(entries)

	fake := newFakeLookup()
	a4 := netip.MustParseAddr("1.2.3.4")
	a6 := netip.MustParseAddr("dead:beef::1")
	fake.push(exampleHost, []netip.Addr{a4, a6}, 60*time.Second)
	fake.setDefault(exampleHost, []netip.Addr{a4, a6}, 60*time.Second)

	res := NewResolver(reg, fake, withClock(newFakeClock()))
	if err := res.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(res.Close)

	waitForSnapshot(t, reg, func(s Snapshot) bool { return len(s.Prefixes) == 2 })

	prefixes := reg.Snapshot().Prefixes
	hasV4, hasV6 := false, false
	for _, p := range prefixes {
		switch {
		case p.Addr() == a4 && p.Bits() == 32:
			hasV4 = true
		case p.Addr() == a6 && p.Bits() == 128:
			hasV6 = true
		}
	}
	if !hasV4 || !hasV6 {
		t.Errorf("prefixes = %v, want /32 for v4 and /128 for v6", prefixes)
	}
}

// TestResolver_MinTTL ensures the loop uses the smallest TTL across
// responses and floors at MinRefreshInterval.
func TestResolver_MinTTL(t *testing.T) {
	entries := mustParse(t, []string{tcpDomain})
	reg := NewRegistry(entries)
	fake := newFakeLookup()
	addr := netip.MustParseAddr("1.2.3.4")
	// Push a tiny TTL — should floor at MinRefreshInterval (30s).
	fake.push(exampleHost, []netip.Addr{addr}, 5*time.Second)
	fake.setDefault(exampleHost, []netip.Addr{addr}, 5*time.Second)

	clk := newFakeClock()
	res := NewResolver(reg, fake, withClock(clk))
	if err := res.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(res.Close)
	waitForSnapshot(t, reg, func(s Snapshot) bool { return len(s.Prefixes) == 1 })

	// Confirm the loop scheduled MinRefreshInterval.
	clk.waitForWaiters(t, 1)
	clk.mu.Lock()
	got := clk.waiters[0].d
	clk.mu.Unlock()
	if got < MinRefreshInterval {
		t.Errorf("scheduled delay = %v, want >= %v", got, MinRefreshInterval)
	}
}

// TestResolver_AddRemoveEvents covers the add-and-remove change-event
// path: pushing two addresses, then one, should fire two distinct
// notifications.
func TestResolver_AddRemoveEvents(t *testing.T) {
	entries := mustParse(t, []string{tcpDomain})
	reg := NewRegistry(entries)
	sub, unsub := reg.Subscribe()
	defer unsub()

	fake := newFakeLookup()
	a1 := netip.MustParseAddr("1.2.3.4")
	a2 := netip.MustParseAddr("5.6.7.8")
	fake.push(exampleHost, []netip.Addr{a1, a2}, 60*time.Second)
	fake.push(exampleHost, []netip.Addr{a1}, 60*time.Second)
	fake.setDefault(exampleHost, []netip.Addr{a1}, 60*time.Second)

	clk := newFakeClock()
	res := NewResolver(reg, fake, withClock(clk))
	if err := res.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(res.Close)

	waitForNotify(t, sub)
	if got := len(reg.Snapshot().Prefixes); got != 2 {
		t.Fatalf("initial prefix count = %d, want 2", got)
	}

	clk.waitForWaiters(t, 1)
	clk.advanceAll()
	waitForNotify(t, sub)
	if got := len(reg.Snapshot().Prefixes); got != 1 {
		t.Errorf("post-removal prefix count = %d, want 1", got)
	}
}

// TestResolver_NoSpuriousNotify confirms a refresh that returns the
// same address set does NOT wake subscribers (coalescing inside the
// registry — see setResolved's sameAddrSet check).
func TestResolver_NoSpuriousNotify(t *testing.T) {
	entries := mustParse(t, []string{tcpDomain})
	reg := NewRegistry(entries)
	sub, unsub := reg.Subscribe()
	defer unsub()

	fake := newFakeLookup()
	addr := netip.MustParseAddr("1.2.3.4")
	fake.push(exampleHost, []netip.Addr{addr}, 60*time.Second)
	fake.push(exampleHost, []netip.Addr{addr}, 60*time.Second)
	fake.setDefault(exampleHost, []netip.Addr{addr}, 60*time.Second)

	clk := newFakeClock()
	res := NewResolver(reg, fake, withClock(clk))
	if err := res.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(res.Close)
	waitForNotify(t, sub)

	clk.waitForWaiters(t, 1)
	clk.advanceAll()
	// Confirm no second notification fires.
	select {
	case <-sub:
		t.Errorf("got spurious notify on unchanged refresh")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestResolver_ErrorRetry ensures an upstream lookup error doesn't
// wipe the previous address set and schedules a retry.
func TestResolver_ErrorRetry(t *testing.T) {
	entries := mustParse(t, []string{tcpDomain})
	reg := NewRegistry(entries)
	fake := newFakeLookup()
	addr := netip.MustParseAddr("1.2.3.4")
	fake.push(exampleHost, []netip.Addr{addr}, 60*time.Second)
	fake.pushErr(exampleHost, errors.New("server misbehaving"))
	fake.setDefault(exampleHost, []netip.Addr{addr}, 60*time.Second)

	clk := newFakeClock()
	res := NewResolver(reg, fake, withClock(clk))
	if err := res.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(res.Close)

	waitForSnapshot(t, reg, func(s Snapshot) bool { return len(s.Prefixes) == 1 })

	clk.waitForWaiters(t, 1)
	clk.advanceAll()
	// After the error, address set is unchanged. Just verify the
	// loop is sleeping again (i.e. a new waiter shows up) without
	// having cleared the snapshot.
	clk.waitForWaiters(t, 1)
	if got := reg.Snapshot().Prefixes[0].Addr(); got != addr {
		t.Errorf("address cleared on error: got %v, want %v", got, addr)
	}
}

// TestResolver_ContextCancellation confirms Close exits all
// goroutines + Start respects parent context cancellation.
func TestResolver_ContextCancellation(t *testing.T) {
	entries := mustParse(t, []string{tcpDomain})
	reg := NewRegistry(entries)
	fake := newFakeLookup()
	fake.setDefault(exampleHost,
		[]netip.Addr{netip.MustParseAddr("1.2.3.4")}, 60*time.Second)

	clk := newFakeClock()
	res := NewResolver(reg, fake, withClock(clk))
	ctx, cancel := context.WithCancel(context.Background())
	if err := res.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Sleep briefly so the goroutine has resolved + entered the
	// After-wait, then cancel — Close should return promptly.
	clk.waitForWaiters(t, 1)
	cancel()
	done := make(chan struct{})
	go func() {
		res.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Close did not return after context cancel")
	}
}

// TestResolver_NoDomainsNoOp ensures Start is a no-op (and won't
// error on nil lookup) when there are no domain entries.
func TestResolver_NoDomainsNoOp(t *testing.T) {
	entries := mustParse(t, []string{"tcp://1.2.3.4:443"})
	reg := NewRegistry(entries)
	res := NewResolver(reg, nil)
	if err := res.Start(context.Background()); err != nil {
		t.Errorf("Start with no domains and nil lookup: %v", err)
	}
	res.Close()
}

// TestSnapshot_Lookup walks the (addr, port, scheme) match table the
// proxy will use at flow-accept time.
func TestSnapshot_Lookup(t *testing.T) {
	entries := mustParse(t, []string{
		tcpCIDR,
		tcpDomain,
		udpDNS,
		"icmp://192.168.0.0/24:8/0",
	})
	reg := NewRegistry(entries)
	reg.setResolved(exampleHost, []netip.Addr{netip.MustParseAddr("9.9.9.9")})

	s := reg.Snapshot()

	cases := []struct {
		name   string
		addr   string
		port   int
		scheme Scheme
		want   string // matched Entry.Original; "" = no match
	}{
		{"cidr match", "10.0.0.5", 443, SchemeTCP, tcpCIDR},
		{"cidr wrong port", "10.0.0.5", 80, SchemeTCP, ""},
		{"domain match", "9.9.9.9", 443, SchemeTCP, tcpDomain},
		{"domain wrong port", "9.9.9.9", 80, SchemeTCP, ""},
		{"udp dns", "100.64.0.1", 53, SchemeUDP, udpDNS},
		{"icmp echo", "192.168.0.5", 0, SchemeICMP, "icmp://192.168.0.0/24:8/0"},
		{"wrong scheme", "10.0.0.5", 443, SchemeUDP, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Lookup(netip.MustParseAddr(tc.addr), tc.port, tc.scheme)
			if tc.want == "" {
				if got != nil {
					t.Errorf("Lookup = %q, want nil", got.Original)
				}
				return
			}
			if got == nil {
				t.Fatalf("Lookup = nil, want %q", tc.want)
			}
			if got.Original != tc.want {
				t.Errorf("Lookup = %q, want %q", got.Original, tc.want)
			}
		})
	}
}

// TestRegistry_LiteralOnlySnapshot confirms entries with no domain
// component show up in the initial Snapshot — no resolver needed.
func TestRegistry_LiteralOnlySnapshot(t *testing.T) {
	entries := mustParse(t, []string{
		tcpCIDR,
		"tcp://192.168.1.5:5432",
	})
	reg := NewRegistry(entries)
	got := reg.Snapshot()
	if len(got.Prefixes) != 2 {
		t.Errorf("len(Prefixes) = %d, want 2", len(got.Prefixes))
	}
}

func mustParse(t *testing.T, strs []string) []Entry {
	t.Helper()
	out, err := Parse(strs)
	if err != nil {
		t.Fatalf("mustParse: %v", err)
	}
	return out
}

func waitForSnapshot(t *testing.T, reg *Registry, pred func(Snapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred(reg.Snapshot()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("snapshot predicate not satisfied within 2s: %+v", reg.Snapshot())
}

func waitForNotify(t *testing.T, sub <-chan struct{}) {
	t.Helper()
	select {
	case <-sub:
	case <-time.After(2 * time.Second):
		t.Fatalf("no notify within 2s")
	}
}
