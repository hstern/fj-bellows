package wg

// Proxy file overview.
//
// Proxy is the tunnel-side TCP bridge for the FJB-54 ACL-driven
// transport. The orchestrator's WireGuard netstack accepts
// connections destined for IPs in the ACL's address set (the
// snapshot's Prefixes); each accepted connection is dispatched via
// the ACL Registry to determine the matching Entry, the upstream to
// dial, and the port-spec to enforce.
//
// Replaces the older per-ProxySpec model (one (listen, upstream) pair
// per operator entry). With the wildcard model the listener set is
// derived purely from the ACL snapshot: the operator declares "what
// workers may reach" and the proxy figures out where to listen.
//
// Listener strategy: gVisor's netstack does not natively support a
// single "any address, any port" listener — gonet.ListenTCP needs an
// explicit (IP, Port). To approximate the design's "wildcard within
// prefix, all TCP ports — port-spec enforcement at accept", the proxy
// enumerates the (address, port) cross-product implied by each tcp
// Entry in the snapshot and opens one netstack listener per pair:
//
//   - addresses come from the entry's resolved set (single-IP literals,
//     single addresses from the resolver loop for domain entries).
//     Multi-IP literal prefixes (e.g. 192.168.10.0/24) are skipped with
//     a warning — those routes terminate on the cache nanode side and
//     are not reachable on the orchestrator's netstack in v1.
//   - ports come from the entry's PortSpec ranges. Wide ranges (more
//     than [DefaultMaxPortsPerEntry] ports) are skipped with a warning
//     so a misconfigured "bare tcp://host" (which the parser expands to
//     1-65535) doesn't try to bind 65k listeners.
//
// On ACL snapshot changes (DNS resolver pushes new addresses, operator
// reloads config) the proxy diffs the listener set: new (addr, port)
// pairs spin up fresh listeners; pairs that disappear close their
// listeners. In-flight connections accepted on a now-removed listener
// continue to run until they close naturally — only the listener
// shuts down, not the bridge goroutines.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hstern/fj-bellows/internal/transport/wg/acl"
)

// DefaultProxyDialTimeout bounds how long the upstream dial may block
// per connection. Sensible default for HTTPS / git-over-ssh — both
// complete TCP handshake well under 10s on a working LAN; anything
// slower than that is likely an upstream-down situation we'd rather
// surface fast than queue connections behind.
const DefaultProxyDialTimeout = 10 * time.Second

// DefaultMaxPortsPerEntry caps the number of ports a single tcp Entry
// is allowed to materialise into listeners. The parser expands a bare
// `tcp://host` (no port-spec) to a single 1-65535 range — without a
// cap we'd try to bind 65k netstack listeners. 64 is comfortably above
// any realistic explicit port set (HTTPS + git ssh + a handful of
// alternates) and bounds resource use to a humane number.
const DefaultMaxPortsPerEntry = 64

// Listener is the subset of *Tunnel we need for opening tunnel-side
// TCP listeners. Defined as an interface so tests can plug in plain
// loopback listeners without bringing up a real WG netstack.
type Listener interface {
	ListenTCP(addr *net.TCPAddr) (net.Listener, error)
}

// Registry is the subset of *acl.Registry the proxy consumes:
// snapshot reads + change notifications. Kept narrow so tests can
// substitute a fake without depending on the resolver loop.
type Registry interface {
	Snapshot() acl.Snapshot
	Subscribe() (<-chan struct{}, func())
}

// Proxy bridges TCP traffic from tunnel-side wildcard listeners to
// upstreams selected per-connection via the ACL Registry. One Proxy
// per orchestrator process.
//
// Construction is cheap; the listener set is computed and opened by
// Start. Close stops every accept loop, force-closes in-flight
// bridges, and waits for goroutines to exit.
type Proxy struct {
	tun      Listener
	registry Registry
	log      *slog.Logger

	dialTimeout time.Duration

	// dialContext is the host-side dialer used to reach upstreams.
	// Indirected for tests (loopback echo servers); production wires
	// net.Dialer{}.DialContext.
	dialContext func(ctx context.Context, network, address string) (net.Conn, error)

	// wg covers ALL goroutines spawned by the proxy: the snapshot-
	// subscribe loop, the per-listener accept loops, and the
	// per-connection bridge goroutines. Close waits on it.
	wg sync.WaitGroup

	// connSeq numbers each accepted connection for log correlation.
	connSeq atomic.Uint64

	// done is closed when the proxy shuts down (Close or parent ctx
	// cancellation). Used as the cancel signal for the snapshot-
	// watch loop and the per-conn dial context.
	done chan struct{}

	mu      sync.Mutex
	started bool
	closed  bool
	unsub   func()
	listens map[listenKey]*activeListen
	conns   map[net.Conn]struct{} // tunnel-side accepted conns
	upConns map[net.Conn]struct{} // matching upstream dials
}

// listenKey identifies one (address, port) listener. Comparable so it
// can be used as a map key for diffing snapshots.
type listenKey struct {
	addr netip.Addr
	port int
}

func (k listenKey) String() string {
	return netip.AddrPortFrom(k.addr, portToUint16(k.port)).String()
}

// portToUint16 narrows a port int to uint16, clamping out-of-range
// values to 0. Ports come from the ACL parser (1-65535) so we never
// see clamping in practice; the helper exists to satisfy gosec's
// integer-overflow check without scattering nolint directives.
func portToUint16(p int) uint16 {
	if p < 0 || p > 65535 {
		return 0
	}
	return uint16(p)
}

// activeListen pairs a netstack listener with the cancel hook that
// stops its accept loop. Closing the listener unblocks Accept; the
// loop checks p.closed / ctx.Err() before deciding whether to log.
type activeListen struct {
	key listenKey
	ln  net.Listener
}

// Option customises Proxy construction.
type Option func(*Proxy)

// WithDialTimeout overrides DefaultProxyDialTimeout on the per-conn
// upstream dial. Zero falls back to the default.
func WithDialTimeout(d time.Duration) Option {
	return func(p *Proxy) {
		if d > 0 {
			p.dialTimeout = d
		}
	}
}

// withDialContext lets tests substitute the host-side dialer. Not
// exported because production has exactly one sensible choice.
func withDialContext(fn func(ctx context.Context, network, address string) (net.Conn, error)) Option {
	return func(p *Proxy) {
		if fn != nil {
			p.dialContext = fn
		}
	}
}

// NewProxy returns a Proxy ready to be Started. tun supplies the
// tunnel-side listener factory; registry drives address-set updates.
// Both are required.
func NewProxy(tun Listener, registry Registry, log *slog.Logger, opts ...Option) (*Proxy, error) {
	if tun == nil {
		return nil, errors.New("wg/proxy: tunnel listener is required")
	}
	if registry == nil {
		return nil, errors.New("wg/proxy: acl registry is required")
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Proxy{
		tun:         tun,
		registry:    registry,
		log:         log,
		dialTimeout: DefaultProxyDialTimeout,
		dialContext: (&net.Dialer{}).DialContext,
		done:        make(chan struct{}),
		listens:     make(map[listenKey]*activeListen),
		conns:       make(map[net.Conn]struct{}),
		upConns:     make(map[net.Conn]struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// Start opens the initial listener set derived from the registry's
// current snapshot, subscribes to change notifications, and returns.
// Returns immediately; the proxy runs until Close is called or ctx
// fires. Safe to call exactly once.
//
// Returns an error only if the initial reconcile fails to open even
// one listener — partial success (some listeners up, some skipped
// with warnings) is not an error.
func (p *Proxy) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("wg/proxy: Start after Close")
	}
	if p.started {
		p.mu.Unlock()
		return errors.New("wg/proxy: already started")
	}
	p.started = true
	p.mu.Unlock()

	ch, unsub := p.registry.Subscribe()
	p.mu.Lock()
	p.unsub = unsub
	p.mu.Unlock()

	// Reconcile once synchronously so callers observe at least the
	// initial listener set before Start returns.
	p.reconcile()

	// Bridge ctx cancellation into the done channel so callers can
	// stop the proxy via either ctx or Close.
	if ctx != nil && ctx.Done() != nil {
		p.wg.Go(func() {
			select {
			case <-ctx.Done():
				_ = p.Close()
			case <-p.done:
			}
		})
	}
	p.wg.Go(func() { p.watchSnapshot(ch) })
	return nil
}

// Close stops every accept loop, force-closes in-flight bridges, and
// waits for all goroutines to exit. Idempotent.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.done)
	unsub := p.unsub
	listens := p.listens
	p.listens = nil
	conns := make([]net.Conn, 0, len(p.conns)+len(p.upConns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	for c := range p.upConns {
		conns = append(conns, c)
	}
	p.mu.Unlock()

	if unsub != nil {
		unsub()
	}
	for _, al := range listens {
		_ = al.ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	p.wg.Wait()
	return nil
}

// watchSnapshot consumes registry change events. Coalesced — every
// signal triggers one reconcile, regardless of how many fired
// in-between.
func (p *Proxy) watchSnapshot(ch <-chan struct{}) {
	for {
		select {
		case <-p.done:
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			p.reconcile()
		}
	}
}

// reconcile computes the desired (addr, port) listener set from the
// current snapshot and diffs against the live set: open new
// listeners, close removed ones. Existing listeners stay untouched.
func (p *Proxy) reconcile() {
	snap := p.registry.Snapshot()
	desired := p.desiredListeners(snap)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	current := p.listens
	// Close listeners that disappeared.
	toClose := make([]*activeListen, 0)
	for k, al := range current {
		if _, keep := desired[k]; !keep {
			toClose = append(toClose, al)
			delete(current, k)
		}
	}
	// Open listeners that appeared.
	toOpen := make([]listenKey, 0)
	for k := range desired {
		if _, exists := current[k]; !exists {
			toOpen = append(toOpen, k)
		}
	}
	p.mu.Unlock()

	for _, al := range toClose {
		p.log.Debug("wg/proxy: closing stale listener", "endpoint", al.key.String())
		_ = al.ln.Close()
	}

	for _, k := range toOpen {
		p.openListener(k)
	}
}

// desiredListeners derives the (addr, port) set the netstack should
// be listening on, given the snapshot's address set + tcp entries.
//
// The algorithm:
//   - Iterate snap.Prefixes (the canonical address set; literals plus
//     resolver-injected /32s for domain entries).
//   - For each single-host prefix, walk snap.Entries to find every tcp
//     Entry whose port-spec, intersected with this address, contributes
//     ports to the listener set.
//
// Multi-IP literal prefixes (e.g. 192.168.10.0/24) are skipped with a
// warning — those routes terminate at the cache nanode's iptables
// FORWARD chain and aren't reachable on the orchestrator's netstack.
// Over-wide port specs (more than DefaultMaxPortsPerEntry ports) are
// also skipped with a warning so a bare `tcp://host` doesn't try to
// bind 65k listeners.
func (p *Proxy) desiredListeners(snap acl.Snapshot) map[listenKey]struct{} {
	out := make(map[listenKey]struct{})
	// Track which entries we've already warned about so a noisy ACL
	// doesn't spam logs on every reconcile.
	multiIPWarned := make(map[string]struct{})
	wideWarned := make(map[string]struct{})

	for _, pfx := range snap.Prefixes {
		if !isSingleHost(pfx) {
			// snap.Prefixes is the union of literal prefixes + each
			// domain entry's resolved /32s. A non-single-host prefix
			// therefore comes from a literal CIDR entry — log once
			// per Original, not per snapshot pass.
			p.warnMultiIPPrefix(snap, pfx, multiIPWarned)
			continue
		}
		addr := pfx.Addr()
		for _, e := range snap.Entries {
			if e.Scheme != acl.SchemeTCP {
				continue
			}
			if !entryMatchesAddr(e, addr, snap) {
				continue
			}
			ports := p.entryPorts(e, wideWarned)
			for _, port := range ports {
				out[listenKey{addr: addr, port: port}] = struct{}{}
			}
		}
	}
	return out
}

// warnMultiIPPrefix logs the matching literal entry once per
// reconcile pass. The seen-set is the caller's so multiple passes
// in one reconcile don't repeat themselves.
func (p *Proxy) warnMultiIPPrefix(snap acl.Snapshot, pfx netip.Prefix, seen map[string]struct{}) {
	for _, e := range snap.Entries {
		if e.IsDomain() {
			continue
		}
		if e.Prefix != pfx {
			continue
		}
		if _, ok := seen[e.Original]; ok {
			return
		}
		seen[e.Original] = struct{}{}
		p.log.Warn(
			"wg/proxy: skipping multi-IP literal entry; orchestrator netstack listens per-host only",
			"entry", e.Original,
			"prefix", pfx.String(),
		)
		return
	}
}

// entryMatchesAddr reports whether addr is reachable through Entry e
// under the current snapshot. Mirrors the address-side half of
// Snapshot.Lookup without the port-spec / scheme guard (caller has
// already filtered to scheme=tcp and walks ports separately).
func entryMatchesAddr(e acl.Entry, addr netip.Addr, snap acl.Snapshot) bool {
	if e.IsDomain() {
		// Lookup uses the entry's PortSpec to confirm; we probe with
		// the entry's first declared port — guaranteed to be in
		// PortSpec by construction.
		port := 1
		if len(e.PortSpec) > 0 {
			port = e.PortSpec[0].Lo
		}
		matched := snap.Lookup(addr, port, acl.SchemeTCP)
		return matched != nil && matched.Host == e.Host
	}
	return e.Prefix.Contains(addr)
}

// entryPorts flattens the entry's PortSpec into a slice of explicit
// ports. Refuses entries whose port set exceeds DefaultMaxPortsPerEntry
// (so a bare `tcp://host` doesn't try to bind 65k listeners). seen
// dedupes the warning across multiple reconcile passes hitting the
// same entry.
func (p *Proxy) entryPorts(e acl.Entry, seen map[string]struct{}) []int {
	total := 0
	for _, r := range e.PortSpec {
		total += r.Hi - r.Lo + 1
		if total > DefaultMaxPortsPerEntry {
			if _, dup := seen[e.Original]; !dup {
				seen[e.Original] = struct{}{}
				p.log.Warn(
					"wg/proxy: skipping entry with too many ports; specify explicit ports",
					"entry", e.Original,
					"max", DefaultMaxPortsPerEntry,
				)
			}
			return nil
		}
	}
	out := make([]int, 0, total)
	for _, r := range e.PortSpec {
		for port := r.Lo; port <= r.Hi; port++ {
			out = append(out, port)
		}
	}
	return out
}

func isSingleHost(p netip.Prefix) bool {
	if !p.IsValid() {
		return false
	}
	return p.Bits() == p.Addr().BitLen()
}

// openListener opens one netstack listener and spawns its accept
// loop. Failures are logged and the listener is skipped — the proxy
// stays up for the other endpoints.
func (p *Proxy) openListener(k listenKey) {
	ln, err := p.tun.ListenTCP(&net.TCPAddr{IP: k.addr.AsSlice(), Port: k.port})
	if err != nil {
		p.log.Warn("wg/proxy: open listener failed", "endpoint", k.String(), "err", err)
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = ln.Close()
		return
	}
	al := &activeListen{key: k, ln: ln}
	p.listens[k] = al
	p.mu.Unlock()

	p.log.Debug("wg/proxy: listener open", "endpoint", k.String())
	p.wg.Go(func() { p.acceptLoop(al) })
}

func (p *Proxy) acceptLoop(al *activeListen) {
	for {
		client, err := al.ln.Accept()
		if err != nil {
			if p.isClosed() {
				return
			}
			// Listener removal from reconcile() is the normal
			// "closed" path; surface as debug, not error.
			p.log.Debug("wg/proxy: accept returned; closing loop", "endpoint", al.key.String(), "err", err)
			return
		}
		p.wg.Go(func() { p.handle(client) })
	}
}

func (p *Proxy) handle(client net.Conn) {
	id := p.connSeq.Add(1)
	clientAddr := client.RemoteAddr().String()
	logger := p.log.With("conn_id", id, "client", clientAddr)

	if !p.trackClient(client) {
		_ = client.Close()
		return
	}
	defer func() {
		p.untrackClient(client)
		_ = client.Close()
	}()

	localAddr, ok := localTCPAddr(client)
	if !ok {
		logger.Warn("wg/proxy: client LocalAddr is not *net.TCPAddr; refusing", "local", client.LocalAddr())
		return
	}
	logger = logger.With("local", localAddr.String())

	snap := p.registry.Snapshot()
	entry := snap.Lookup(localAddr.Addr(), int(localAddr.Port()), acl.SchemeTCP)
	if entry == nil {
		logger.Warn("wg/proxy: no ACL match; refusing connection")
		return
	}
	logger = logger.With("entry", entry.Original)

	upstreamHostPort := upstreamFor(entry, localAddr)

	baseCtx, cancelBase := p.dialCtx(context.Background())
	dialCtx, cancel := context.WithTimeout(baseCtx, p.dialTimeout)
	up, err := p.dialContext(dialCtx, "tcp", upstreamHostPort)
	cancel()
	cancelBase()
	if err != nil {
		logger.Warn("wg/proxy: upstream dial failed", "upstream", upstreamHostPort, "err", err)
		return
	}
	if !p.trackUpstream(up) {
		_ = up.Close()
		return
	}
	defer func() {
		p.untrackUpstream(up)
		_ = up.Close()
	}()

	logger.Debug("wg/proxy: connection bridged", "upstream", upstreamHostPort)
	startedAt := time.Now()
	clientToUpstream, upstreamToClient := bridge(client, up)
	logger.Debug(
		"wg/proxy: connection closed",
		"bytes_client_to_upstream", clientToUpstream,
		"bytes_upstream_to_client", upstreamToClient,
		"dur", time.Since(startedAt).String(),
	)
}

// upstreamFor picks the host:port string to dial for a matched
// entry. Literal-IP entries dial back to the same (addr, port) on
// the host network: the netstack accepted the connection because
// the cache routed worker-VPC traffic to that destination, and the
// orchestrator just plays it forward.
//
// Domain entries dial entry.Host:port so the host resolver picks
// the current authoritative IP (which may differ from the netstack-
// resolved address set under split-horizon DNS).
func upstreamFor(e *acl.Entry, local netip.AddrPort) string {
	portStr := strconv.Itoa(int(local.Port()))
	if e.IsDomain() {
		return net.JoinHostPort(e.Host, portStr)
	}
	return net.JoinHostPort(local.Addr().String(), portStr)
}

// localTCPAddr extracts the netip.AddrPort from a conn's LocalAddr.
// Returns ok=false for non-TCP conns or addresses that fail to
// parse as netip.AddrPort (defensive — netstack and stdlib both
// return *net.TCPAddr).
func localTCPAddr(c net.Conn) (netip.AddrPort, bool) {
	tcp, ok := c.LocalAddr().(*net.TCPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	addr, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return netip.AddrPort{}, false
	}
	// netip.AddrFromSlice returns a 4-in-6 mapping for IPv4
	// addresses sent through a 16-byte slice; unwrap so Lookup
	// (which works in plain v4 / v6 space) matches the ACL prefix.
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	return netip.AddrPortFrom(addr, portToUint16(tcp.Port)), true
}

// bridge copies bytes both ways between client and upstream until
// both directions complete. When one direction's Copy returns, the
// other side gets CloseWrite (when supported) so the FIN propagates
// promptly. Returns per-direction byte counts.
func bridge(client, upstream net.Conn) (clientToUpstream, upstreamToClient int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(upstream, client)
		clientToUpstream = n
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(client, upstream)
		upstreamToClient = n
		closeWrite(client)
	}()
	wg.Wait()
	return clientToUpstream, upstreamToClient
}

// closeWrite signals "no more bytes from this direction" on conns
// that support half-close. TCP conns implement it; many fake/wrapped
// conns don't — the type assertion silently no-ops for those.
func closeWrite(c net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

func (p *Proxy) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// dialCtx returns a context that's cancelled when the proxy shuts
// down — so an in-flight upstream dial unblocks immediately on
// Close rather than waiting out the full dial timeout.
func (p *Proxy) dialCtx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := make(chan struct{})
	go func() {
		select {
		case <-p.done:
			cancel()
		case <-stop:
		}
	}()
	return ctx, func() {
		close(stop)
		cancel()
	}
}

func (p *Proxy) trackClient(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) untrackClient(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

func (p *Proxy) trackUpstream(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.upConns[c] = struct{}{}
	return true
}

func (p *Proxy) untrackUpstream(c net.Conn) {
	p.mu.Lock()
	delete(p.upConns, c)
	p.mu.Unlock()
}
