// Package dns implements a small DNS responder that runs inside the
// orchestrator's wireguard-go netstack at 100.64.0.1:53 (UDP+TCP).
//
// The responder is the cache nanode's view-of-the-world for name
// resolution: queries from the cache (and the workers behind it, via
// the cache's resolver chain) terminate here. Two paths:
//
//  1. Internal hit — QNAME matches an entry in the InternalTable
//     (currently just "cache" → cache.vpc_ip; future entries seeded by
//     FJB-82's ACL resolver loop). Synthesize an A reply with a short
//     TTL so re-binding is fast.
//
//  2. Miss — recurse through Go's default resolver
//     (net.DefaultResolver.LookupNetIP), which honours the orchestrator
//     host's normal resolver chain (systemd-resolved, /etc/resolv.conf,
//     etc). Build A or AAAA based on the query type. SERVFAIL on error.
//
// Library is golang.org/x/net/dns/dnsmessage — byte-level, allocation-
// conscious, no DNS-server framework. ~200 lines including the TCP
// length-prefix path.
package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DefaultListenAddr is the netstack-side bind address for the responder.
// Both UDP and TCP listeners attach here. The address is chosen so the
// cache nanode's resolver can be pointed at it via static config (it
// lives in the orchestrator's tunnel-side address space).
const DefaultListenAddr = "100.64.0.1:53"

// DefaultInternalTTL is the TTL on synthesized A records for internal
// names. Short on purpose: when the operator changes cache.vpc_ip, the
// cache and workers should re-resolve within ~30s instead of carrying a
// stale binding for hours.
const DefaultInternalTTL = 30 * time.Second

// DefaultUDPMessageSize is the recv buffer per UDP read. RFC 1035 caps
// UDP DNS at 512 bytes without EDNS0; we round up to 1232 (the common
// EDNS0 "no fragmentation" payload size). The responder doesn't
// advertise EDNS0 itself but oversized incoming queries shouldn't crash
// the parser.
const DefaultUDPMessageSize = 1232

// tcpReadTimeout bounds how long a single TCP query may block on read.
// Stops slowloris-style connection holds from pinning responder
// goroutines. DNS-over-TCP is meant to be one query per dial in
// practice; if a client wants more it can re-dial.
const tcpReadTimeout = 5 * time.Second

// InternalTable resolves an internal name (without trailing dot) to a
// netip.Addr. Implementations must be safe for concurrent use — the
// responder calls Lookup from every query-handling goroutine.
//
// The intent is that FJB-82 / FJB-90 wiring populates a single struct
// implementing this interface with a mutex-protected map driven by
// managedCache + ACL config. This package stays decoupled by depending
// only on the small interface.
type InternalTable interface {
	// Lookup returns the canonical IP for an internal name. The name is
	// lower-cased and has no trailing dot. Returns ok=false when the
	// name is not internal (the responder will then forward to the host
	// resolver).
	Lookup(name string) (netip.Addr, bool)
}

// HostResolver resolves a public name through the orchestrator host's
// resolver chain. Mirrors net.DefaultResolver.LookupNetIP so production
// can pass net.DefaultResolver directly. Tests inject a fake.
type HostResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Listener is the netstack-listen surface the responder needs. The
// wireguard-go netstack returns concrete *gonet types that satisfy
// these stdlib interfaces — orchestrator wiring constructs a small
// adapter (NetstackListener below) so this package doesn't import
// gvisor / wireguard-go directly.
type Listener interface {
	// ListenPacket binds a UDP listener.
	ListenPacket(network, address string) (net.PacketConn, error)

	// Listen binds a TCP listener.
	Listen(network, address string) (net.Listener, error)
}

// Config configures a Responder.
type Config struct {
	// ListenAddr is the bind address shared by the UDP and TCP
	// listeners. Zero value uses DefaultListenAddr.
	ListenAddr string

	// InternalTTL is the TTL on synthesized A records. Zero value uses
	// DefaultInternalTTL (30s).
	InternalTTL time.Duration

	// Listener provides netstack-side ListenPacket / Listen. Required.
	Listener Listener

	// Table is the internal-name lookup table. Required.
	Table InternalTable

	// Host resolves names not in Table. Zero value uses
	// net.DefaultResolver — the orchestrator host's normal chain
	// (systemd-resolved, /etc/resolv.conf, etc).
	Host HostResolver

	// Logger receives per-query Debug events and listener-level Warn /
	// Error events. Zero value uses slog.Default.
	Logger *slog.Logger
}

// Responder is a DNS responder bound on the netstack side. Start spawns
// the UDP recv loop and the TCP accept loop; Close stops both and waits
// for in-flight handlers.
type Responder struct {
	listenAddr  string
	internalTTL uint32
	table       InternalTable
	host        HostResolver
	listener    Listener
	log         *slog.Logger

	mu     sync.Mutex
	udp    net.PacketConn
	tcp    net.Listener
	closed bool
	wg     sync.WaitGroup
}

// New constructs a Responder. Validation is synchronous so config
// errors surface at startup instead of from a goroutine.
func New(cfg Config) (*Responder, error) {
	if cfg.Listener == nil {
		return nil, errors.New("dns: Listener is required")
	}
	if cfg.Table == nil {
		return nil, errors.New("dns: Table is required")
	}
	addr := cfg.ListenAddr
	if addr == "" {
		addr = DefaultListenAddr
	}
	ttl := cfg.InternalTTL
	if ttl == 0 {
		ttl = DefaultInternalTTL
	}
	host := cfg.Host
	if host == nil {
		host = net.DefaultResolver
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Responder{
		listenAddr:  addr,
		internalTTL: uint32(ttl.Seconds()),
		table:       cfg.Table,
		host:        host,
		listener:    cfg.Listener,
		log:         log,
	}, nil
}

// Start binds both listeners and spawns their loops. Returns once the
// listeners are bound — bind failures surface synchronously. Safe to
// call exactly once.
func (r *Responder) Start(ctx context.Context) error {
	udp, err := r.listener.ListenPacket("udp", r.listenAddr)
	if err != nil {
		return fmt.Errorf("dns: ListenPacket %s: %w", r.listenAddr, err)
	}
	tcp, err := r.listener.Listen("tcp", r.listenAddr)
	if err != nil {
		_ = udp.Close()
		return fmt.Errorf("dns: Listen %s: %w", r.listenAddr, err)
	}

	r.mu.Lock()
	r.udp = udp
	r.tcp = tcp
	r.mu.Unlock()

	r.log.Info("dns responder up", "addr", r.listenAddr)

	r.wg.Go(func() { r.udpLoop(ctx, udp) })
	r.wg.Go(func() { r.tcpLoop(ctx, tcp) })
	return nil
}

// Close stops the listeners and waits for in-flight handlers to drain.
// Idempotent.
func (r *Responder) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	udp, tcp := r.udp, r.tcp
	r.mu.Unlock()

	if udp != nil {
		_ = udp.Close()
	}
	if tcp != nil {
		_ = tcp.Close()
	}
	r.wg.Wait()
	return nil
}

// udpLoop reads packets serially and dispatches each to a handler
// goroutine. UDP is connectionless so we can't backpressure — but DNS
// queries are tiny and the responder is on a private tunnel, so
// unbounded spawning is acceptable for now.
func (r *Responder) udpLoop(ctx context.Context, conn net.PacketConn) {
	buf := make([]byte, DefaultUDPMessageSize)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if r.isClosed() {
				return
			}
			r.log.Warn("dns udp read", "err", err)
			return
		}
		// Copy: the dispatched goroutine outlives buf's next read.
		query := make([]byte, n)
		copy(query, buf[:n])
		r.wg.Go(func() {
			resp := r.handle(ctx, query)
			if resp == nil {
				return
			}
			if _, err := conn.WriteTo(resp, addr); err != nil && !r.isClosed() {
				r.log.Debug("dns udp write", "err", err, "addr", addr)
			}
		})
	}
}

// tcpLoop accepts TCP connections and dispatches each to a per-conn
// handler. Per RFC 1035 §4.2.2 each TCP query is framed with a 2-byte
// big-endian length prefix; a single connection may carry multiple
// queries serially.
func (r *Responder) tcpLoop(ctx context.Context, listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if r.isClosed() {
				return
			}
			r.log.Warn("dns tcp accept", "err", err)
			return
		}
		r.wg.Go(func() {
			defer func() { _ = conn.Close() }()
			r.handleTCP(ctx, conn)
		})
	}
}

func (r *Responder) handleTCP(ctx context.Context, conn net.Conn) {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(tcpReadTimeout))
		var lenBuf [2]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			if !errors.Is(err, io.EOF) && !r.isClosed() {
				r.log.Debug("dns tcp read len", "err", err)
			}
			return
		}
		qlen := binary.BigEndian.Uint16(lenBuf[:])
		if qlen == 0 {
			return
		}
		query := make([]byte, qlen)
		if _, err := io.ReadFull(conn, query); err != nil {
			r.log.Debug("dns tcp read body", "err", err)
			return
		}
		resp := r.handle(ctx, query)
		if resp == nil {
			return
		}
		// DNS over TCP messages are 16-bit length-prefixed; a response
		// longer than 65535 bytes can't be framed and shouldn't ever be
		// produced by this responder (small answer set, no zone xfer).
		if len(resp) > 0xFFFF {
			r.log.Warn("dns tcp response too large", "len", len(resp))
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(tcpReadTimeout))
		var out [2]byte
		binary.BigEndian.PutUint16(out[:], uint16(len(resp))) //nolint:gosec // bounds-checked: len(resp) ≤ 0xFFFF above
		if _, err := conn.Write(out[:]); err != nil {
			return
		}
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}

// handle parses one query and returns the wire-format response. Returns
// nil when the query is so malformed we can't even extract a header /
// question to echo back — there's nothing to reply to in that case.
func (r *Responder) handle(ctx context.Context, query []byte) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		r.log.Debug("dns parse header", "err", err)
		return nil
	}
	q, err := parser.Question()
	if err != nil {
		r.log.Debug("dns parse question", "err", err)
		// Build a FORMERR response so the client doesn't hang.
		return buildErrorResponse(header, dnsmessage.RCodeFormatError, dnsmessage.Question{})
	}
	name := canonicalize(q.Name.String())

	if ip, ok := r.table.Lookup(name); ok && ip.IsValid() {
		// Family mismatch (e.g. AAAA query against an A-only entry)
		// becomes NOERROR with empty answer per RFC 4074.
		return buildAddrResponse(header, q, []netip.Addr{ip}, r.internalTTL)
	}

	// Miss: forward through host resolver. Use a per-query deadline so
	// a slow upstream doesn't pin handler goroutines forever.
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	network := "ip4"
	if q.Type == dnsmessage.TypeAAAA {
		network = "ip6"
	}
	addrs, err := r.host.LookupNetIP(lookupCtx, network, strings.TrimSuffix(name, "."))
	if err != nil {
		// Distinguish "not found" from a real resolver failure. Go
		// surfaces NXDOMAIN through *net.DNSError with IsNotFound=true.
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return buildErrorResponse(header, dnsmessage.RCodeNameError, q)
		}
		r.log.Debug("dns host resolve", "name", name, "err", err)
		return buildErrorResponse(header, dnsmessage.RCodeServerFailure, q)
	}
	if len(addrs) == 0 {
		return buildErrorResponse(header, dnsmessage.RCodeNameError, q)
	}

	// Build a response carrying every matching address of the requested
	// family.
	return buildAddrResponse(header, q, addrs, r.internalTTL)
}

// canonicalize lower-cases and trims a domain name to the form used as
// the internal-table key (no trailing dot). dnsmessage.Name.String()
// always includes a trailing dot; we strip it here.
func canonicalize(name string) string {
	name = strings.TrimSuffix(name, ".")
	return strings.ToLower(name)
}

func responseHeader(req dnsmessage.Header, rcode dnsmessage.RCode) dnsmessage.Header {
	return dnsmessage.Header{
		ID:                 req.ID,
		Response:           true,
		OpCode:             req.OpCode,
		Authoritative:      false,
		RecursionDesired:   req.RecursionDesired,
		RecursionAvailable: true,
		RCode:              rcode,
	}
}

func buildErrorResponse(req dnsmessage.Header, rcode dnsmessage.RCode, q dnsmessage.Question) []byte {
	b := dnsmessage.NewBuilder(nil, responseHeader(req, rcode))
	b.EnableCompression()
	// Best-effort echo of the question section. If question is zero
	// (FORMERR path) skip it — clients still recover from QDCOUNT=0
	// when RCODE != 0.
	if q.Name.Length > 0 {
		if err := b.StartQuestions(); err == nil {
			_ = b.Question(q)
		}
	}
	msg, err := b.Finish()
	if err != nil {
		return nil
	}
	return msg
}

// addAnswer appends one A or AAAA record for addr to the in-progress
// builder. Returns true on success. Skips silently when addr doesn't
// match the question type.
func addAnswer(b *dnsmessage.Builder, q dnsmessage.Question, addr netip.Addr, ttl uint32) bool {
	switch {
	case q.Type == dnsmessage.TypeA && addr.Is4():
		err := b.AResource(
			dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: q.Class, TTL: ttl},
			dnsmessage.AResource{A: addr.As4()},
		)
		return err == nil
	case q.Type == dnsmessage.TypeAAAA && addr.Is6() && !addr.Is4():
		err := b.AAAAResource(
			dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeAAAA, Class: q.Class, TTL: ttl},
			dnsmessage.AAAAResource{AAAA: addr.As16()},
		)
		return err == nil
	}
	return false
}

// buildAddrResponse returns a NOERROR response carrying each addr in
// addrs whose family matches q.Type. When no address matches, returns
// NOERROR with an empty answer section (RFC 4074) — the correct
// behaviour for "this family has nothing", distinct from NXDOMAIN.
func buildAddrResponse(req dnsmessage.Header, q dnsmessage.Question, addrs []netip.Addr, ttl uint32) []byte {
	b := dnsmessage.NewBuilder(nil, responseHeader(req, dnsmessage.RCodeSuccess))
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil
	}
	if err := b.Question(q); err != nil {
		return nil
	}
	if err := b.StartAnswers(); err != nil {
		return nil
	}
	for _, addr := range addrs {
		_ = addAnswer(&b, q, addr, ttl)
	}
	msg, err := b.Finish()
	if err != nil {
		return nil
	}
	return msg
}

func (r *Responder) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// MapTable is a trivial InternalTable backed by an immutable map. Useful
// for tests and for the initial single-entry production wiring before
// FJB-82 lands its dynamic resolver.
type MapTable map[string]netip.Addr

// Lookup implements InternalTable. Names are lower-cased before lookup.
func (m MapTable) Lookup(name string) (netip.Addr, bool) {
	a, ok := m[strings.ToLower(name)]
	return a, ok
}
