package icmp

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xicmp "golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// testUpstream is the RFC 5737 TEST-NET-1 address used in fixture
// lookups; never resolves anywhere real, but a stable string keeps
// the test assertions readable.
const testUpstream = "192.0.2.1"

// fakePingAddr satisfies the structural addrProvider interface used in
// pingAddrToNetip plus net.Addr — mirrors *netstack.PingAddr's shape
// without pulling in the netstack dep.
type fakePingAddr struct{ a netip.Addr }

func (p *fakePingAddr) Network() string  { return "ping4" }
func (p *fakePingAddr) String() string   { return p.a.String() }
func (p *fakePingAddr) Addr() netip.Addr { return p.a }

// fakePingConn is a channel-backed bidirectional PingConn for tests.
// inbound writes (via Inject) become ReadFrom returns; outbound writes
// (via WriteTo) are captured on Replies.
type fakePingConn struct {
	local   netip.Addr
	in      chan inboundPkt
	replies chan replyPkt

	mu     sync.Mutex
	closed bool
}

type inboundPkt struct {
	data []byte
	src  net.Addr
}

type replyPkt struct {
	data []byte
	dst  net.Addr
}

func newFakePingConn(local netip.Addr) *fakePingConn {
	return &fakePingConn{
		local:   local,
		in:      make(chan inboundPkt, 16),
		replies: make(chan replyPkt, 16),
	}
}

func (f *fakePingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	pkt, ok := <-f.in
	if !ok {
		return 0, nil, net.ErrClosed
	}
	n := copy(p, pkt.data)
	return n, pkt.src, nil
}

func (f *fakePingConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return 0, net.ErrClosed
	}
	f.mu.Unlock()
	cp := make([]byte, len(p))
	copy(cp, p)
	f.replies <- replyPkt{data: cp, dst: addr}
	return len(p), nil
}

func (f *fakePingConn) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	close(f.in)
	f.mu.Unlock()
	return nil
}

func (f *fakePingConn) LocalAddr() net.Addr {
	return &fakePingAddr{a: f.local}
}

func (f *fakePingConn) Inject(pkt inboundPkt) {
	f.in <- pkt
}

func mustMarshalEcho(t *testing.T, typ ipv4.ICMPType, id, seq int, data []byte) []byte {
	t.Helper()
	m := &xicmp.Message{Type: typ, Code: 0, Body: &xicmp.Echo{ID: id, Seq: seq, Data: data}}
	b, err := m.Marshal(nil)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestBridge_EchoRoundTrip(t *testing.T) {
	local := netip.MustParseAddr("10.99.0.1")
	src := netip.MustParseAddr("10.99.0.5")
	pc := newFakePingConn(local)

	originate := func(_ context.Context, _ string, id, seq int, data []byte) ([]byte, int, int, error) {
		// Echo the data back unchanged (and rewrite id like the kernel
		// unprivileged socket would).
		return append([]byte(nil), data...), id, seq, nil
	}
	lookup := func(addr netip.Addr) (string, bool) {
		if addr != local {
			t.Errorf("unexpected dst: %v", addr)
		}
		return testUpstream, true
	}

	br, err := NewBridge(pc, originate, lookup, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	br.Start()
	t.Cleanup(func() { _ = br.Close() })

	payload := []byte("fjb-84-icmp-payload")
	req := mustMarshalEcho(t, ipv4.ICMPTypeEcho, 0x4242, 7, payload)
	pc.Inject(inboundPkt{data: req, src: &fakePingAddr{a: src}})

	select {
	case reply := <-pc.replies:
		// Reply must be addressed to the original requester.
		got, ok := reply.dst.(*fakePingAddr)
		if !ok || got.a != src {
			t.Errorf("reply dst = %v, want %v", reply.dst, src)
		}
		// Parse and validate the reply payload.
		msg, err := xicmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), reply.data)
		if err != nil {
			t.Fatalf("parse reply: %v", err)
		}
		if msg.Type != ipv4.ICMPTypeEchoReply {
			t.Errorf("reply type = %v, want EchoReply", msg.Type)
		}
		echo, ok := msg.Body.(*xicmp.Echo)
		if !ok {
			t.Fatalf("reply body = %T, want *xicmp.Echo", msg.Body)
		}
		if echo.ID != 0x4242 || echo.Seq != 7 {
			t.Errorf("reply id/seq = %d/%d, want 0x4242/7", echo.ID, echo.Seq)
		}
		if string(echo.Data) != string(payload) {
			t.Errorf("reply data = %q, want %q", echo.Data, payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no reply produced within 3s")
	}
}

func TestBridge_RejectsNonEcho(t *testing.T) {
	local := netip.MustParseAddr("10.99.0.1")
	src := netip.MustParseAddr("10.99.0.5")
	pc := newFakePingConn(local)

	var originateCalls atomic.Int32
	originate := func(_ context.Context, _ string, id, seq int, data []byte) ([]byte, int, int, error) {
		originateCalls.Add(1)
		return data, id, seq, nil
	}
	lookup := func(_ netip.Addr) (string, bool) { return testUpstream, true }

	br, err := NewBridge(pc, originate, lookup, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	br.Start()
	t.Cleanup(func() { _ = br.Close() })

	// Inject a Timestamp request (type 13) — must be rejected.
	pkt := mustMarshalEcho(t, ipv4.ICMPTypeTimestamp, 1, 1, []byte("nope"))
	pc.Inject(inboundPkt{data: pkt, src: &fakePingAddr{a: src}})

	select {
	case reply := <-pc.replies:
		t.Errorf("expected no reply for non-echo type; got %d bytes", len(reply.data))
	case <-time.After(200 * time.Millisecond):
		// expected: no reply
	}
	if originateCalls.Load() != 0 {
		t.Errorf("originate called %d times on non-echo; want 0", originateCalls.Load())
	}
}

func TestBridge_DeniedByACL(t *testing.T) {
	local := netip.MustParseAddr("10.99.0.1")
	src := netip.MustParseAddr("10.99.0.5")
	pc := newFakePingConn(local)

	var originateCalls atomic.Int32
	originate := func(_ context.Context, _ string, id, seq int, data []byte) ([]byte, int, int, error) {
		originateCalls.Add(1)
		return data, id, seq, nil
	}
	lookup := func(_ netip.Addr) (string, bool) { return "", false }

	br, err := NewBridge(pc, originate, lookup, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	br.Start()
	t.Cleanup(func() { _ = br.Close() })

	for i := range 3 {
		pkt := mustMarshalEcho(t, ipv4.ICMPTypeEcho, 1, i, []byte("x"))
		pc.Inject(inboundPkt{data: pkt, src: &fakePingAddr{a: src}})
	}
	select {
	case reply := <-pc.replies:
		t.Errorf("expected no reply on denied dst; got %d bytes", len(reply.data))
	case <-time.After(200 * time.Millisecond):
		// expected
	}
	if originateCalls.Load() != 0 {
		t.Errorf("originate called %d times on deny; want 0", originateCalls.Load())
	}
}

func TestBridge_UpstreamTimeoutDoesNotReply(t *testing.T) {
	local := netip.MustParseAddr("10.99.0.1")
	src := netip.MustParseAddr("10.99.0.5")
	pc := newFakePingConn(local)

	originate := func(ctx context.Context, _ string, _, _ int, _ []byte) ([]byte, int, int, error) {
		<-ctx.Done()
		return nil, 0, 0, ctx.Err()
	}
	lookup := func(_ netip.Addr) (string, bool) { return testUpstream, true }

	br, err := NewBridge(pc, originate, lookup,
		slog.New(slog.DiscardHandler),
		WithUpstreamTimeout(50*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	br.Start()
	t.Cleanup(func() { _ = br.Close() })

	pkt := mustMarshalEcho(t, ipv4.ICMPTypeEcho, 1, 1, []byte("slow"))
	pc.Inject(inboundPkt{data: pkt, src: &fakePingAddr{a: src}})

	select {
	case reply := <-pc.replies:
		t.Errorf("expected no reply on timeout; got %d bytes", len(reply.data))
	case <-time.After(500 * time.Millisecond):
		// expected
	}
}

func TestBridge_RejectsBadConstructorArgs(t *testing.T) {
	pc := newFakePingConn(netip.MustParseAddr("10.99.0.1"))
	originate := func(_ context.Context, _ string, id, seq int, data []byte) ([]byte, int, int, error) {
		return data, id, seq, nil
	}
	lookup := func(_ netip.Addr) (string, bool) { return "", false }

	if _, err := NewBridge(nil, originate, lookup, nil); err == nil {
		t.Error("nil PingConn should error")
	}
	if _, err := NewBridge(pc, nil, lookup, nil); err == nil {
		t.Error("nil originate should error")
	}
	if _, err := NewBridge(pc, originate, nil, nil); err == nil {
		t.Error("nil lookup should error")
	}
}

func TestBridge_CloseIsIdempotent(t *testing.T) {
	pc := newFakePingConn(netip.MustParseAddr("10.99.0.1"))
	br, err := NewBridge(pc,
		func(_ context.Context, _ string, id, seq int, data []byte) ([]byte, int, int, error) {
			return data, id, seq, nil
		},
		func(_ netip.Addr) (string, bool) { return "", false },
		slog.New(slog.DiscardHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	br.Start()
	if err := br.Close(); err != nil {
		t.Errorf("first close: %v", err)
	}
	if err := br.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestBridge_OriginateErrorIsLoggedNotPanic(t *testing.T) {
	pc := newFakePingConn(netip.MustParseAddr("10.99.0.1"))
	originate := func(_ context.Context, _ string, _, _ int, _ []byte) ([]byte, int, int, error) {
		return nil, 0, 0, errors.New("upstream gone")
	}
	br, err := NewBridge(pc, originate,
		func(_ netip.Addr) (string, bool) { return testUpstream, true },
		slog.New(slog.DiscardHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	br.Start()
	t.Cleanup(func() { _ = br.Close() })

	pkt := mustMarshalEcho(t, ipv4.ICMPTypeEcho, 1, 1, []byte("x"))
	pc.Inject(inboundPkt{data: pkt, src: &fakePingAddr{a: netip.MustParseAddr("10.99.0.5")}})

	select {
	case <-pc.replies:
		t.Error("expected no reply when originate errors")
	case <-time.After(200 * time.Millisecond):
		// expected
	}
}
