package acl

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"
)

// MinRefreshInterval floors the per-entry DNS refresh cadence. The
// design (transport.md § Domain resolution) says re-resolve at the
// rrset's min(TTL); short TTLs like 5s would otherwise spin tight
// loops against `net.DefaultResolver` and amplify upstream load.
// 30s matches the responder's outgoing TTL and gives operators a
// predictable lower bound on propagation.
const MinRefreshInterval = 30 * time.Second

// DefaultRefreshOnError is how long the resolver waits before retrying
// a domain whose last lookup failed. Tight enough that transient
// resolver hiccups recover quickly; loose enough not to hammer a
// genuinely-broken upstream.
const DefaultRefreshOnError = 30 * time.Second

// LookupResult is one DNS resolution outcome — a set of IPs from
// merged A and AAAA answers plus the rrset min(TTL).
type LookupResult struct {
	Addrs []netip.Addr
	// TTL is the rrset min across A + AAAA. A zero value tells the
	// Resolver to fall back to MinRefreshInterval.
	TTL time.Duration
}

// Lookup is the contract Resolver uses to talk to DNS. Production code
// passes a thin wrapper around net.DefaultResolver + dnsmessage to
// recover TTL; tests pass an in-memory fake. The Resolver does NOT
// itself care which.
type Lookup interface {
	LookupHost(ctx context.Context, host string) (LookupResult, error)
}

// Snapshot is an immutable view of the registry at one instant —
// resolved address set + the typed entries that produced them.
type Snapshot struct {
	// Prefixes is the union of every literal CIDR plus every domain
	// resolution's /32 or /128, deduplicated, sorted for stable
	// comparison.
	Prefixes []netip.Prefix

	// Entries is every parsed Entry the snapshot was derived from
	// (literal + domain). The order matches the registry's entry
	// order (operator-first per DedupeRaw).
	Entries []Entry

	// resolved is per-domain-host the current address set. Used by
	// Lookup() to attribute an inbound address to a domain entry.
	resolved map[string][]netip.Addr
}

// Lookup finds an Entry that matches the (addr, port, scheme) tuple,
// or nil if none does. Used by the proxy on accept to decide whether
// to dial and on which port-spec to enforce.
//
// Matching rules:
//
//   - Scheme must match (case-sensitive lower).
//   - Address must be inside Entry.Prefix (literal) or in the
//     resolved address set for a domain entry.
//   - For tcp/udp entries: port must fall inside any of the
//     Entry.PortSpec ranges.
//   - For icmp entries: port is ignored (callers pass 0). The runtime
//     icmp-type/code check belongs to the icmp bridge, not here.
//
// Returns the first matching entry. Operators get config-order
// precedence; implicit entries (which DedupeRaw appended last) act
// as fall-throughs.
func (s Snapshot) Lookup(addr netip.Addr, port int, scheme Scheme) *Entry {
	if !addr.IsValid() {
		return nil
	}
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.Scheme != scheme {
			continue
		}
		if !snapshotMatchAddr(e, addr, s.resolved) {
			continue
		}
		if e.Scheme == SchemeICMP {
			return e
		}
		for _, r := range e.PortSpec {
			if r.Contains(port) {
				return e
			}
		}
	}
	return nil
}

func snapshotMatchAddr(e *Entry, addr netip.Addr, resolved map[string][]netip.Addr) bool {
	if e.IsDomain() {
		return slices.Contains(resolved[e.Host], addr)
	}
	return e.Prefix.Contains(addr)
}

// Registry holds the live address set + entry list and broadcasts
// change events to subscribers. The orchestrator owns one Registry;
// callers (proxy, cache renderer, worker cloud-init builder) read via
// Snapshot.
type Registry struct {
	mu        sync.Mutex
	entries   []Entry
	resolved  map[string][]netip.Addr
	current   Snapshot
	subs      map[int]chan struct{}
	subNextID int
}

// NewRegistry builds a Registry seeded with the parsed entries.
// Literal entries contribute their Prefix immediately; domain entries
// contribute nothing until the Resolver fills them in (or never, if
// the caller doesn't start one).
func NewRegistry(entries []Entry) *Registry {
	r := &Registry{
		entries:  append([]Entry(nil), entries...),
		resolved: make(map[string][]netip.Addr),
		subs:     make(map[int]chan struct{}),
	}
	r.rebuildLocked()
	return r
}

// Snapshot returns the current view. Safe to call concurrently and
// from any goroutine; the returned value is immutable.
func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// Subscribe returns a channel that gets a non-blocking send on every
// Snapshot change, plus an unsubscribe func. Subscribers get
// coalesced notifications — a slow consumer doesn't accumulate
// backlog (the channel has buffer 1 and we drop sends when full).
func (r *Registry) Subscribe() (<-chan struct{}, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.subNextID
	r.subNextID++
	ch := make(chan struct{}, 1)
	r.subs[id] = ch
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
	}
}

// setResolved is the resolver's hook into the registry. It updates the
// address set for one host and broadcasts only if something actually
// changed (so a no-op refresh doesn't wake every subscriber).
func (r *Registry) setResolved(host string, addrs []netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.resolved[host]
	if sameAddrSet(prev, addrs) {
		return
	}
	if len(addrs) == 0 {
		delete(r.resolved, host)
	} else {
		// Defensive copy + canonical sort so equality checks are cheap.
		cp := append([]netip.Addr(nil), addrs...)
		sort.Slice(cp, func(i, j int) bool { return cp[i].Less(cp[j]) })
		r.resolved[host] = cp
	}
	r.rebuildLocked()
	r.notifyLocked()
}

func (r *Registry) rebuildLocked() {
	seen := make(map[netip.Prefix]struct{})
	prefixes := make([]netip.Prefix, 0, len(r.entries))
	add := func(p netip.Prefix) {
		if !p.IsValid() {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		prefixes = append(prefixes, p)
	}
	for _, e := range r.entries {
		if !e.IsDomain() {
			add(e.Prefix)
			continue
		}
		for _, a := range r.resolved[e.Host] {
			bits := a.BitLen()
			add(netip.PrefixFrom(a, bits))
		}
	}
	sort.Slice(prefixes, func(i, j int) bool {
		ai, aj := prefixes[i].Addr(), prefixes[j].Addr()
		if ai != aj {
			return ai.Less(aj)
		}
		return prefixes[i].Bits() < prefixes[j].Bits()
	})

	// Copy resolved map so Snapshot is immutable.
	resolved := make(map[string][]netip.Addr, len(r.resolved))
	for k, v := range r.resolved {
		resolved[k] = append([]netip.Addr(nil), v...)
	}
	r.current = Snapshot{
		Prefixes: prefixes,
		Entries:  append([]Entry(nil), r.entries...),
		resolved: resolved,
	}
}

func (r *Registry) notifyLocked() {
	for _, ch := range r.subs {
		select {
		case ch <- struct{}{}:
		default:
			// Subscriber hasn't drained — they'll see the latest
			// snapshot when they do. Coalescing is intentional.
		}
	}
}

func sameAddrSet(a, b []netip.Addr) bool {
	if len(a) != len(b) {
		return false
	}
	// Both come in sorted (after the first setResolved canonicalises).
	// For the very first call prev is nil so len comparison short-circuits.
	aa := append([]netip.Addr(nil), a...)
	bb := append([]netip.Addr(nil), b...)
	sort.Slice(aa, func(i, j int) bool { return aa[i].Less(aa[j]) })
	sort.Slice(bb, func(i, j int) bool { return bb[i].Less(bb[j]) })
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

// Resolver spawns one goroutine per domain entry, looking up A + AAAA
// records via the injected Lookup interface and pushing results into
// the Registry. Refresh cadence per host is max(rrset-min-TTL,
// MinRefreshInterval). Errors retry at DefaultRefreshOnError.
//
// One Resolver per Registry. Construct, Start, and Close in that
// order. Close blocks until every per-host goroutine has returned.
type Resolver struct {
	reg     *Registry
	lookup  Lookup
	clock   clock // injectable for tests
	domains []string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ResolverOption customises Resolver construction. The only knob today
// is a test-only clock override (withClock); kept as an option type so
// adding production knobs later (per-host retry policy, max
// concurrency) doesn't break the constructor signature.
type ResolverOption func(*Resolver)

// NewResolver builds a Resolver wired to the registry. lookup may be
// nil — in which case Start is a no-op (useful when the operator has
// no domain entries).
func NewResolver(reg *Registry, lookup Lookup, opts ...ResolverOption) *Resolver {
	r := &Resolver{
		reg:    reg,
		lookup: lookup,
		clock:  realClock{},
	}
	for _, opt := range opts {
		opt(r)
	}
	seen := make(map[string]struct{})
	for _, e := range reg.entries {
		if !e.IsDomain() {
			continue
		}
		if _, ok := seen[e.Host]; ok {
			continue
		}
		seen[e.Host] = struct{}{}
		r.domains = append(r.domains, e.Host)
	}
	return r
}

// Start fires off the per-host goroutines and returns. Cancel via
// Close. Returns nil if there's nothing to do.
func (r *Resolver) Start(ctx context.Context) error {
	if len(r.domains) == 0 {
		return nil
	}
	if r.lookup == nil {
		return errors.New("acl: Resolver has domain entries but no Lookup")
	}
	ctx, r.cancel = context.WithCancel(ctx)
	for _, host := range r.domains {
		r.wg.Add(1)
		go r.runHost(ctx, host)
	}
	return nil
}

// Close stops every goroutine and waits for them to exit. Safe to call
// multiple times; safe to call before Start.
func (r *Resolver) Close() {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
}

func (r *Resolver) runHost(ctx context.Context, host string) {
	defer r.wg.Done()
	for {
		res, err := r.lookup.LookupHost(ctx, host)
		var delay time.Duration
		if err != nil {
			// On error keep the previous addrs (already in the
			// registry); just schedule a retry. A persistent error
			// will eventually surface via missing reachability —
			// users see DNS errors at dial time, which is the
			// expected and debuggable behaviour.
			delay = DefaultRefreshOnError
		} else {
			r.reg.setResolved(host, res.Addrs)
			delay = max(res.TTL, MinRefreshInterval)
		}

		select {
		case <-ctx.Done():
			return
		case <-r.clock.After(delay):
		}
	}
}

// clock is the time abstraction the resolver loop uses for sleeps.
// Tests inject a fake to control refresh cadence deterministically.
type clock interface {
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
