// Package icmp implements an ACL-driven ICMP echo bridge running on
// the tunnel-side netstack (FJB-84, part of the FJB-54 transport
// redesign).
//
// v1 is intentionally narrow: it bridges ICMPv4 type 8 (echo request) /
// type 0 (echo reply) and rejects every other type with a logged drop.
// The ACL grammar tolerates richer icmpspec strings (FJB-82), but the
// runtime restricts to {0, 8}.
//
// Bridge model: inbound echo requests arrive on the netstack-side
// PingConn (laddr = orchestrator's tunnel address). For each request,
// the bridge calls a caller-supplied OriginateFn to send the matching
// echo against the host network (typically via golang.org/x/net/icmp's
// unprivileged DGRAM socket on Linux), reads back the reply, and
// mirrors it (same id/seq/data) back through the netstack to the
// original requester.
//
// This package takes callbacks for ACL dispatch and outbound origin
// so it stays decoupled from the ACL registry (FJB-82) and the DNS
// resolver (FJB-83). The orchestrator wires them up (FJB-90).
//
// IPv6 (type 128/129) is not implemented in v1 — see the TODO on
// Bridge.dispatch. The wire-level shape would be identical with
// IPv6PseudoHeader passed to icmp.Message.Marshal; the orchestrator
// has no IPv6 workers in the current architecture.
package icmp

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	xicmp "golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// DefaultUpstreamTimeout bounds how long the bridge waits for the
// upstream echo reply before giving up on the request. 5s comfortably
// covers any non-pathological round trip; pings to a working host
// complete in milliseconds.
const DefaultUpstreamTimeout = 5 * time.Second

// DefaultReadBufferSize bounds inbound ICMP message size. 1500 covers
// any Ethernet-MTU-bound echo payload; ICMP doesn't carry larger
// datagrams under any practical configuration.
const DefaultReadBufferSize = 1500

// LookupFn resolves the inbound packet's destination address to an
// upstream "host" (no port — ICMP is portless). Returns ok=false to
// drop the packet (caller-side ACL deny).
//
// localAddr is the destination IP of the inbound ICMP echo request.
// The bridge does not interpret upstream beyond passing it to
// OriginateFn.
type LookupFn func(localAddr netip.Addr) (upstream string, ok bool)

// OriginateFn sends one ICMPv4 echo request to the given upstream and
// returns the parsed echo reply. The orchestrator wires up a function
// backed by golang.org/x/net/icmp.ListenPacket("udp4", ...) (uses the
// Linux unprivileged ICMP DGRAM socket, no CAP_NET_RAW).
//
// id and seq from the inbound request are passed through verbatim;
// the implementation may rewrite id (Linux unprivileged ICMP sockets
// stamp their own kernel-assigned id), in which case the returned
// reply must carry the inbound id/seq for the bridge to mirror back.
type OriginateFn func(ctx context.Context, upstream string, id, seq int, data []byte) (replyData []byte, replyID, replySeq int, err error)

// PingConn is the netstack-side ICMP listener. Aliased for docs; in
// production this is a *netstack.PingConn returned by
// (*netstack.Net).ListenPingAddr.
type PingConn interface {
	ReadFrom(p []byte) (n int, addr net.Addr, err error)
	WriteTo(p []byte, addr net.Addr) (n int, err error)
	Close() error
	LocalAddr() net.Addr
}

// Bridge handles inbound ICMP echo requests on the tunnel side and
// mirrors replies from the host network back to the requester.
// Close stops the read loop and waits for in-flight requests to
// finish.
type Bridge struct {
	in        PingConn
	originate OriginateFn
	lookup    LookupFn
	log       *slog.Logger

	upstreamTimeout time.Duration
	readBuf         int

	pingSeq atomic.Uint64

	wg sync.WaitGroup

	mu     sync.Mutex
	closed bool

	// dropOnce: per-source-IP denied logging. Same reasoning as the
	// UDP forwarder — keep log volume bounded under floods.
	dropMu   sync.Mutex
	dropSeen map[netip.Addr]struct{}
}

// Option configures a Bridge.
type Option func(*Bridge)

// WithUpstreamTimeout overrides DefaultUpstreamTimeout.
func WithUpstreamTimeout(d time.Duration) Option {
	return func(b *Bridge) { b.upstreamTimeout = d }
}

// WithReadBufferSize overrides DefaultReadBufferSize.
func WithReadBufferSize(n int) Option {
	return func(b *Bridge) { b.readBuf = n }
}

// NewBridge constructs a Bridge. The caller must Start it.
// `in`, `originate`, and `lookup` are required; log defaults to
// slog.Default.
func NewBridge(in PingConn, originate OriginateFn, lookup LookupFn, log *slog.Logger, opts ...Option) (*Bridge, error) {
	if in == nil {
		return nil, errors.New("wg/icmp: PingConn is required")
	}
	if originate == nil {
		return nil, errors.New("wg/icmp: OriginateFn is required")
	}
	if lookup == nil {
		return nil, errors.New("wg/icmp: LookupFn is required")
	}
	if log == nil {
		log = slog.Default()
	}
	b := &Bridge{
		in:              in,
		originate:       originate,
		lookup:          lookup,
		log:             log,
		upstreamTimeout: DefaultUpstreamTimeout,
		readBuf:         DefaultReadBufferSize,
		dropSeen:        make(map[netip.Addr]struct{}),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b, nil
}

// Start spawns the inbound read loop. Returns immediately; the bridge
// runs until Close is called. Safe to call exactly once.
func (b *Bridge) Start() {
	b.wg.Go(b.readLoop)
}

// Close stops accepting new ICMP messages and waits for all in-flight
// originate calls to finish. Idempotent.
func (b *Bridge) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()
	_ = b.in.Close()
	b.wg.Wait()
	return nil
}

func (b *Bridge) readLoop() {
	buf := make([]byte, b.readBuf)
	for {
		n, src, err := b.in.ReadFrom(buf)
		if err != nil {
			if b.isClosed() {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			b.log.Warn("wg/icmp: read from netstack failed", "err", err)
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		b.wg.Go(func() { b.dispatch(pkt, src) })
	}
}

// dispatch parses one ICMP message, validates its type, looks up the
// destination, originates against the host network, and mirrors the
// reply back to the requester through the netstack.
//
// TODO: IPv6 echo (type 128/129). x/net/icmp can parse both; the
// difference is the pseudo-header passed to Marshal. wireguard-go's
// netstack already supports ICMPv6 via DialPingAddr on an IPv6 addr.
// Defer until the orchestrator gets an IPv6 path through the tunnel.
func (b *Bridge) dispatch(pkt []byte, src net.Addr) {
	srcAddr, ok := pingAddrToNetip(src)
	if !ok {
		b.log.Warn("wg/icmp: dropping packet — unrecognized src addr type", "src", src)
		return
	}
	localAddr, ok := pingAddrToNetip(b.in.LocalAddr())
	if !ok {
		b.log.Warn("wg/icmp: dropping packet — unrecognized local addr type",
			"local", b.in.LocalAddr())
		return
	}

	msg, err := xicmp.ParseMessage(ipv4.ICMPTypeEcho.Protocol(), pkt)
	if err != nil {
		b.log.Warn("wg/icmp: parse failed", "err", err, "src", srcAddr.String())
		return
	}
	if msg.Type != ipv4.ICMPTypeEcho {
		// v1 explicitly rejects everything but echo request. Echo
		// replies arriving inbound aren't ours to bridge — they'd be
		// for an originating echo we didn't send.
		var typeNum int
		if v4, ok := msg.Type.(ipv4.ICMPType); ok {
			typeNum = int(v4)
		}
		b.log.Info("wg/icmp: rejecting non-echo ICMP type",
			"src", srcAddr.String(),
			"type", typeNum,
		)
		return
	}
	echo, ok := msg.Body.(*xicmp.Echo)
	if !ok {
		b.log.Warn("wg/icmp: echo request has unexpected body shape", "src", srcAddr.String())
		return
	}

	upstream, ok := b.lookup(localAddr)
	if !ok {
		b.logDeniedOnce(srcAddr, localAddr)
		return
	}

	id := b.pingSeq.Add(1)
	logger := b.log.With(
		"ping_id", id,
		"src", srcAddr.String(),
		"dst", localAddr.String(),
		"upstream", upstream,
		"icmp_id", echo.ID,
		"icmp_seq", echo.Seq,
	)
	logger.Debug("wg/icmp: echo request received")

	ctx, cancel := context.WithTimeout(context.Background(), b.upstreamTimeout)
	defer cancel()
	startedAt := time.Now()
	replyData, _, _, err := b.originate(ctx, upstream, echo.ID, echo.Seq, echo.Data)
	if err != nil {
		logger.Warn("wg/icmp: upstream echo failed", "err", err, "dur", time.Since(startedAt).String())
		return
	}

	reply := &xicmp.Message{
		Type: ipv4.ICMPTypeEchoReply,
		Code: 0,
		Body: &xicmp.Echo{
			ID:   echo.ID,
			Seq:  echo.Seq,
			Data: replyData,
		},
	}
	replyBytes, err := reply.Marshal(nil)
	if err != nil {
		logger.Warn("wg/icmp: marshal reply failed", "err", err)
		return
	}
	if _, err := b.in.WriteTo(replyBytes, src); err != nil {
		logger.Warn("wg/icmp: write reply to netstack failed", "err", err)
		return
	}
	logger.Debug("wg/icmp: echo reply mirrored",
		"bytes_request", len(echo.Data),
		"bytes_reply", len(replyData),
		"dur", time.Since(startedAt).String(),
	)
}

func (b *Bridge) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// logDeniedOnce keeps deny-log volume bounded under floods. Each
// distinct source IP gets exactly one "denied" line for the bridge's
// lifetime. (UDP's per-flow eviction lets the dedup table reset on
// timeout; ICMP has no flow, so the table grows monotonically — but
// the cardinality is bounded by the size of the IPv4 space the
// operator routes through the tunnel, which is tiny in practice.)
func (b *Bridge) logDeniedOnce(srcAddr, localAddr netip.Addr) {
	b.dropMu.Lock()
	if _, seen := b.dropSeen[srcAddr]; seen {
		b.dropMu.Unlock()
		return
	}
	b.dropSeen[srcAddr] = struct{}{}
	b.dropMu.Unlock()
	b.log.Info("wg/icmp: echo request denied by ACL",
		"src", srcAddr.String(),
		"dst", localAddr.String(),
	)
}

// pingAddrToNetip extracts a netip.Addr from a wireguard-go PingAddr
// (or any net.Addr exposing String() of a parseable IP).
func pingAddrToNetip(a net.Addr) (netip.Addr, bool) {
	if a == nil {
		return netip.Addr{}, false
	}
	// netstack.PingAddr exposes Addr() via duck-typing — but it lives
	// in the netstack package so we can't import it without creating
	// a hard dep. Use a structural interface assertion instead.
	type addrProvider interface{ Addr() netip.Addr }
	if ap, ok := a.(addrProvider); ok {
		got := ap.Addr()
		if got.IsValid() {
			return got.Unmap(), true
		}
	}
	parsed, err := netip.ParseAddr(a.String())
	if err != nil {
		return netip.Addr{}, false
	}
	return parsed.Unmap(), true
}
