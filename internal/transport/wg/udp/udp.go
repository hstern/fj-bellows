// Package udp implements an ACL-driven per-flow UDP forwarder running on
// the tunnel-side netstack (FJB-84, part of the FJB-54 transport
// redesign).
//
// The forwarder is the UDP analogue of internal/transport/wg/proxy.go:
// it accepts packets on a wildcard netstack-side PacketConn, looks up
// the destination via an ACL callback, and bridges packets to a
// per-flow upstream PacketConn on the host network. Flows are tracked
// by the inbound 5-tuple; an idle timer evicts flows after a
// configurable timeout (default 60s).
//
// This package deliberately takes callbacks for ACL dispatch and
// upstream dial — it knows nothing about the ACL registry (FJB-82) or
// the DNS resolver (FJB-83). The orchestrator wires them up (FJB-90).
package udp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultIdleTimeout is the per-flow inactivity window after which a
// UDP flow is evicted. Chosen to match the conntrack UDP "stream"
// default on Linux (60s after a packet has flowed in each direction).
// In practice this is the right order of magnitude for any UDP app
// that bothers to multiplex on a single 5-tuple (DNS, QUIC, WireGuard
// handshakes themselves); flows that don't get traffic in a minute
// almost certainly aren't coming back without the client re-binding.
const DefaultIdleTimeout = 60 * time.Second

// DefaultReadBufferSize bounds a single inbound packet. 64 KiB is the
// theoretical max UDP datagram (IPv4 16-bit length field minus IP +
// UDP headers); using the full ceiling means we never truncate.
const DefaultReadBufferSize = 65535

// LookupFn resolves the inbound packet's destination address to an
// upstream "host:port" string. Returns ok=false to drop the packet
// (caller-side ACL deny).
//
// localAddr is the dst IP of the inbound packet (extracted from the
// PacketConn's local address); port is the dst port. The forwarder
// does not interpret upstream beyond passing it to DialFn.
type LookupFn func(localAddr netip.Addr, port int) (upstream string, ok bool)

// DialFn opens a host-network UDP socket toward addr. The orchestrator
// supplies a net.Dialer-backed implementation; tests can stub. The
// returned net.Conn must already be connected — the forwarder issues
// Read/Write against it without re-specifying a remote address.
type DialFn func(ctx context.Context, network, addr string) (net.Conn, error)

// PacketConnIn is the netstack-side wildcard listener the forwarder
// reads from. Aliased to net.PacketConn for documentation; the
// orchestrator passes in a *gonet.UDPConn (netstack ListenUDP result).
type PacketConnIn = net.PacketConn

// Forwarder owns one inbound wildcard PacketConn, a flow-table, and
// the upstream sockets backing each flow. Close stops the read loop,
// force-closes every flow, and waits for all goroutines to finish.
type Forwarder struct {
	in     PacketConnIn
	dial   DialFn
	lookup LookupFn
	log    *slog.Logger

	idleTimeout time.Duration
	readBuf     int
	dialTimeout time.Duration

	now func() time.Time // injected for tests

	flowSeq atomic.Uint64

	wg sync.WaitGroup

	mu     sync.Mutex
	closed bool
	flows  map[flowKey]*flow

	// dropOnce: per-source 5-tuple denied logging. A misbehaving client
	// can flood log volume with one packet per ms; we want exactly one
	// "denied" log line per source until the flow times out of the map.
	dropMu   sync.Mutex
	dropSeen map[flowKey]struct{}
}

// Option configures a Forwarder. Use the With* helpers; we don't
// expose the struct fields directly so future options stay additive.
type Option func(*Forwarder)

// WithIdleTimeout overrides the default idle-eviction window.
func WithIdleTimeout(d time.Duration) Option {
	return func(f *Forwarder) { f.idleTimeout = d }
}

// WithReadBufferSize overrides the inbound packet buffer size.
// Useful only if the caller knows datagrams are bounded — saves memory
// on memory-constrained deployments. Default 65535 is always safe.
func WithReadBufferSize(n int) Option {
	return func(f *Forwarder) { f.readBuf = n }
}

// WithDialTimeout bounds the per-flow upstream Dial. Zero disables the
// timeout (uses the dial function's own behavior).
func WithDialTimeout(d time.Duration) Option {
	return func(f *Forwarder) { f.dialTimeout = d }
}

// WithNowFunc injects a clock for deterministic tests. Production
// callers should not use this; time.Now is the default.
func WithNowFunc(now func() time.Time) Option {
	return func(f *Forwarder) { f.now = now }
}

// NewForwarder constructs a Forwarder. The caller must Start it.
// `in`, `dial`, and `lookup` are required; log defaults to slog.Default.
func NewForwarder(in PacketConnIn, dial DialFn, lookup LookupFn, log *slog.Logger, opts ...Option) (*Forwarder, error) {
	if in == nil {
		return nil, errors.New("wg/udp: PacketConnIn is required")
	}
	if dial == nil {
		return nil, errors.New("wg/udp: DialFn is required")
	}
	if lookup == nil {
		return nil, errors.New("wg/udp: LookupFn is required")
	}
	if log == nil {
		log = slog.Default()
	}
	f := &Forwarder{
		in:          in,
		dial:        dial,
		lookup:      lookup,
		log:         log,
		idleTimeout: DefaultIdleTimeout,
		readBuf:     DefaultReadBufferSize,
		dialTimeout: 10 * time.Second,
		now:         time.Now,
		flows:       make(map[flowKey]*flow),
		dropSeen:    make(map[flowKey]struct{}),
	}
	for _, opt := range opts {
		opt(f)
	}
	return f, nil
}

// Start spawns the inbound read loop. Returns immediately; the
// forwarder runs until Close is called. Safe to call exactly once.
func (f *Forwarder) Start() {
	f.wg.Go(f.readLoop)
}

// Close stops the read loop, force-closes every in-flight flow, and
// waits for all goroutines to finish. Idempotent.
func (f *Forwarder) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	flows := make([]*flow, 0, len(f.flows))
	for _, fl := range f.flows {
		flows = append(flows, fl)
	}
	f.flows = nil
	f.mu.Unlock()

	_ = f.in.Close()
	for _, fl := range flows {
		fl.close("forwarder-close")
	}
	f.wg.Wait()
	return nil
}

func (f *Forwarder) readLoop() {
	buf := make([]byte, f.readBuf)
	for {
		n, src, err := f.in.ReadFrom(buf)
		if err != nil {
			if f.isClosed() {
				return
			}
			// PacketConn ReadFrom on a netstack UDPConn surfaces
			// EOF / closed-network errors when the device shuts down;
			// don't spin on those.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			f.log.Warn("wg/udp: read from netstack failed", "err", err)
			return
		}
		// Copy: buf is reused on the next iteration.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		f.dispatch(pkt, src)
	}
}

// dispatch finds or creates the flow for src and writes the packet to
// its upstream.
func (f *Forwarder) dispatch(pkt []byte, src net.Addr) {
	srcAP, dstAP, ok := extractAddrs(src, f.in.LocalAddr())
	if !ok {
		f.log.Warn("wg/udp: dropping packet — unrecognized addr type",
			"src", src, "local", f.in.LocalAddr())
		return
	}
	key := flowKey{
		srcIP:   srcAP.Addr(),
		srcPort: srcAP.Port(),
		dstIP:   dstAP.Addr(),
		dstPort: dstAP.Port(),
	}

	fl, created, err := f.flowFor(key, srcAP, dstAP)
	if err != nil {
		f.logDeniedOnce(key, srcAP, dstAP, err)
		return
	}
	if created {
		f.log.Debug("wg/udp: flow opened",
			"flow_id", fl.id,
			"src", srcAP.String(),
			"dst", dstAP.String(),
			"upstream", fl.upstream,
		)
	}
	fl.sendToUpstream(pkt)
}

// flowFor returns the flow for key, creating it (and the upstream
// PacketConn + reply goroutine) on first use. Returns an error if the
// ACL denies the destination or the upstream dial fails.
func (f *Forwarder) flowFor(key flowKey, srcAP, dstAP netip.AddrPort) (*flow, bool, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, false, errors.New("forwarder closed")
	}
	if fl, ok := f.flows[key]; ok {
		fl.touch(f.now())
		f.mu.Unlock()
		return fl, false, nil
	}
	f.mu.Unlock()

	upstream, ok := f.lookup(dstAP.Addr(), int(dstAP.Port()))
	if !ok {
		return nil, false, errACLDeny
	}

	ctx := context.Background()
	var cancel context.CancelFunc
	if f.dialTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, f.dialTimeout)
	}
	up, err := f.dial(ctx, "udp", upstream)
	if cancel != nil {
		cancel()
	}
	if err != nil {
		return nil, false, fmt.Errorf("upstream dial: %w", err)
	}

	id := f.flowSeq.Add(1)
	fl := &flow{
		id:        id,
		key:       key,
		src:       srcAP,
		dst:       dstAP,
		upstream:  upstream,
		up:        up,
		in:        f.in,
		startedAt: f.now(),
		lastSeen:  f.now(),
		log:       f.log.With("flow_id", id, "src", srcAP.String(), "dst", dstAP.String(), "upstream", upstream),
		readBuf:   f.readBuf,
		now:       f.now,
		onEvict:   func() { f.evict(key) },
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = up.Close()
		return nil, false, errors.New("forwarder closed")
	}
	// Race: another goroutine might have created the same flow while
	// we were dialing. Drop ours, use theirs.
	if existing, dup := f.flows[key]; dup {
		f.mu.Unlock()
		_ = up.Close()
		existing.touch(f.now())
		return existing, false, nil
	}
	f.flows[key] = fl
	f.mu.Unlock()

	fl.startTimer(f.idleTimeout)
	f.wg.Go(fl.upstreamToClientLoop)
	return fl, true, nil
}

// evict removes the flow from the table on idle eviction or close.
func (f *Forwarder) evict(key flowKey) {
	f.mu.Lock()
	fl, ok := f.flows[key]
	if ok {
		delete(f.flows, key)
	}
	f.mu.Unlock()

	f.dropMu.Lock()
	delete(f.dropSeen, key)
	f.dropMu.Unlock()

	if ok {
		fl.close("idle")
	}
}

// isClosed returns true after Close has begun.
func (f *Forwarder) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// logDeniedOnce ensures the "denied" log line fires once per (src,dst)
// per flow lifetime — a packet-rate flood can't blow up the log buffer.
func (f *Forwarder) logDeniedOnce(key flowKey, srcAP, dstAP netip.AddrPort, err error) {
	f.dropMu.Lock()
	if _, seen := f.dropSeen[key]; seen {
		f.dropMu.Unlock()
		return
	}
	f.dropSeen[key] = struct{}{}
	f.dropMu.Unlock()

	if errors.Is(err, errACLDeny) {
		f.log.Info("wg/udp: packet denied by ACL",
			"src", srcAP.String(),
			"dst", dstAP.String(),
		)
		return
	}
	f.log.Warn("wg/udp: packet dropped",
		"src", srcAP.String(),
		"dst", dstAP.String(),
		"err", err,
	)
}

// errACLDeny is the sentinel returned when LookupFn returns ok=false.
// Distinct error so logDeniedOnce can pick the right log level.
var errACLDeny = errors.New("acl deny")

// flowKey is the (src-IP, src-port, dst-IP, dst-port) tuple used as
// the flow-table key. netip.Addr is comparable so this struct is too.
type flowKey struct {
	srcIP   netip.Addr
	srcPort uint16
	dstIP   netip.Addr
	dstPort uint16
}

// flow is one per (srcAP -> dstAP) NAT entry. It owns the upstream
// PacketConn, the idle timer, and the upstream->client read goroutine.
type flow struct {
	id        uint64
	key       flowKey
	src       netip.AddrPort
	dst       netip.AddrPort
	upstream  string
	up        net.Conn
	in        net.PacketConn
	startedAt time.Time
	log       *slog.Logger
	readBuf   int
	now       func() time.Time
	onEvict   func()

	bytesOut atomic.Uint64 // client -> upstream
	bytesIn  atomic.Uint64 // upstream -> client

	mu        sync.Mutex
	lastSeen  time.Time
	timer     *time.Timer
	closed    bool
	idleAfter time.Duration // set in startTimer; touch() resets the timer to fire after this duration of additional idle.
}

func (fl *flow) touch(at time.Time) {
	fl.mu.Lock()
	fl.lastSeen = at
	t := fl.timer
	fl.mu.Unlock()
	if t != nil {
		// Reset is safe here even if the timer just fired — the timer
		// callback re-checks lastSeen under the lock and re-arms when
		// the flow has seen traffic since it was scheduled.
		t.Reset(fl.idleWindow())
	}
}

// idleWindow returns the configured idle timeout. Reset on every
// packet restores the full window, matching the kernel conntrack
// contract for UDP "stream" flows.
func (fl *flow) idleWindow() time.Duration {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.idleAfter
}

func (fl *flow) startTimer(idle time.Duration) {
	fl.mu.Lock()
	fl.idleAfter = idle
	fl.mu.Unlock()
	fl.timer = time.AfterFunc(idle, fl.onTimer)
}

func (fl *flow) onTimer() {
	fl.mu.Lock()
	if fl.closed {
		fl.mu.Unlock()
		return
	}
	// Recompute: if lastSeen + idleAfter is still in the future, the
	// flow was touched mid-flight — re-arm and don't evict.
	remaining := fl.lastSeen.Add(fl.idleAfter).Sub(fl.now())
	if remaining > 0 {
		t := fl.timer
		fl.mu.Unlock()
		if t != nil {
			t.Reset(remaining)
		}
		return
	}
	fl.mu.Unlock()
	fl.onEvict()
}

// sendToUpstream writes one packet to the upstream socket. Logs at
// Warn on partial-write or error; the protocol on top (DNS, QUIC, app)
// is responsible for retransmit.
func (fl *flow) sendToUpstream(pkt []byte) {
	fl.touch(fl.now())
	n, err := fl.up.Write(pkt)
	if err != nil {
		if errors.Is(err, net.ErrClosed) {
			return
		}
		fl.log.Warn("wg/udp: upstream write failed", "err", err, "len", len(pkt))
		return
	}
	fl.bytesOut.Add(uint64(n)) //nolint:gosec // n >= 0 from Write
}

// upstreamToClientLoop reads replies from the upstream socket and
// writes them back to the original requester through the netstack.
// Returns when the upstream socket is closed (flow evicted, forwarder
// shutting down, or upstream-side hangup).
func (fl *flow) upstreamToClientLoop() {
	buf := make([]byte, fl.readBuf)
	for {
		n, err := fl.up.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Treat any other read error as terminal for this flow —
			// matches the kernel-conntrack model (ICMP unreach,
			// upstream gone, etc). The next inbound packet from the
			// same src will reopen the flow.
			fl.log.Debug("wg/udp: upstream read returned", "err", err)
			return
		}
		fl.bytesIn.Add(uint64(n)) //nolint:gosec // n >= 0 from Read
		fl.touch(fl.now())

		// Write back to the client through the netstack-side socket.
		// The src/dst of the original inbound packet are inverted: we
		// write to fl.src (the original requester) FROM fl.dst's port.
		// Netstack handles the src-port rewrite automatically because
		// the inbound PacketConn was bound on a wildcard port.
		clientAddr := &net.UDPAddr{
			IP:   fl.src.Addr().AsSlice(),
			Port: int(fl.src.Port()),
		}
		if _, err := fl.in.WriteTo(buf[:n], clientAddr); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			fl.log.Warn("wg/udp: client write failed", "err", err, "len", n)
			// Don't bail — the netstack PacketConn is shared across all
			// flows; one transient error doesn't kill the forwarder.
		}
	}
}

// close tears down the flow: stops the idle timer, closes the upstream
// socket (which unblocks upstreamToClientLoop), and emits the close
// log line with flow stats. Idempotent.
func (fl *flow) close(reason string) {
	fl.mu.Lock()
	if fl.closed {
		fl.mu.Unlock()
		return
	}
	fl.closed = true
	t := fl.timer
	fl.timer = nil
	fl.mu.Unlock()

	if t != nil {
		t.Stop()
	}
	_ = fl.up.Close()
	fl.log.Debug("wg/udp: flow closed",
		"reason", reason,
		"bytes_out", fl.bytesOut.Load(),
		"bytes_in", fl.bytesIn.Load(),
		"dur", fl.now().Sub(fl.startedAt).String(),
	)
}

// extractAddrs converts net.Addrs from the netstack PacketConn into
// netip.AddrPort pairs. Returns ok=false for unexpected addr types
// (which should be unreachable in production — gonet.UDPConn always
// returns *net.UDPAddr).
func extractAddrs(src, local net.Addr) (srcAP, dstAP netip.AddrPort, ok bool) {
	srcUDP, sok := src.(*net.UDPAddr)
	dstUDP, dok := local.(*net.UDPAddr)
	if !sok || !dok {
		return netip.AddrPort{}, netip.AddrPort{}, false
	}
	srcA, sok2 := netip.AddrFromSlice(srcUDP.IP)
	dstA, dok2 := netip.AddrFromSlice(dstUDP.IP)
	if !sok2 || !dok2 {
		return netip.AddrPort{}, netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(srcA.Unmap(), uint16(srcUDP.Port)), //nolint:gosec // UDP ports are 0..65535
		netip.AddrPortFrom(dstA.Unmap(), uint16(dstUDP.Port)), //nolint:gosec // UDP ports are 0..65535
		true
}
