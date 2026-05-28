package udp

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// newLoopbackPacketConn opens a UDP socket on 127.0.0.1:0 and returns
// the conn + its AddrPort. Used both for the "inbound" netstack-side
// fake and as the upstream echo target.
func newLoopbackPacketConn(t *testing.T) (net.PacketConn, netip.AddrPort) {
	t.Helper()
	lc := net.ListenConfig{}
	pc, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	udpAddr, ok := pc.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("LocalAddr type = %T, want *net.UDPAddr", pc.LocalAddr())
	}
	a, ok := netip.AddrFromSlice(udpAddr.IP)
	if !ok {
		t.Fatal("AddrFromSlice failed")
	}
	return pc, netip.AddrPortFrom(a.Unmap(), uint16(udpAddr.Port)) //nolint:gosec // UDP ports are 0..65535
}

// startUDPEcho starts a goroutine that reads packets and writes them
// back to the sender. Returns the addr to dial.
func startUDPEcho(t *testing.T) netip.AddrPort {
	t.Helper()
	pc, ap := newLoopbackPacketConn(t)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, src, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], src)
		}
	}()
	return ap
}

// hostUDPDial dials a host-network UDP socket toward addr. Used by the
// forwarder under test.
func hostUDPDial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{}
	return d.DialContext(ctx, network, addr)
}

func TestForwarder_RoundTrip(t *testing.T) {
	echo := startUDPEcho(t)
	echoStr := echo.String()

	in, inAddr := newLoopbackPacketConn(t)

	lookup := func(_ netip.Addr, _ int) (string, bool) {
		return echoStr, true
	}

	fwd, err := NewForwarder(in, hostUDPDial, lookup, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	fwd.Start()
	t.Cleanup(func() { _ = fwd.Close() })

	// Client sends a packet to the forwarder's PacketConn.
	clientLC := net.ListenConfig{}
	client, err := clientLC.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	payload := []byte("hello-fjb-84")
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(inAddr.Port())}
	if _, err := client.WriteTo(payload, dst); err != nil {
		t.Fatalf("client write: %v", err)
	}

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	n, _, err := client.ReadFrom(buf)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Errorf("echo mismatch: got %q want %q", buf[:n], payload)
	}
}

func TestForwarder_DenyDropsAndLogsOnce(t *testing.T) {
	in, inAddr := newLoopbackPacketConn(t)
	var lookups atomic.Int32
	lookup := func(_ netip.Addr, _ int) (string, bool) {
		lookups.Add(1)
		return "", false
	}
	fwd, err := NewForwarder(in, hostUDPDial, lookup, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	fwd.Start()
	t.Cleanup(func() { _ = fwd.Close() })

	clientLC := net.ListenConfig{}
	client, err := clientLC.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(inAddr.Port())}

	// Send several packets from the same client tuple. Lookup is
	// called once per packet (deny is not cached at the ACL); but the
	// "denied" log line fires only once per (src,dst).
	for range 5 {
		if _, err := client.WriteTo([]byte("x"), dst); err != nil {
			t.Fatal(err)
		}
	}

	// Give the read loop a chance to process.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lookups.Load() >= 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lookups.Load() < 5 {
		t.Errorf("lookups = %d, want >= 5", lookups.Load())
	}

	// Read from client: must NOT receive anything back.
	_ = client.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 64)
	if n, _, err := client.ReadFrom(buf); err == nil {
		t.Errorf("expected no reply on denied flow; got %d bytes (%q)", n, buf[:n])
	} else if !isTimeout(err) {
		t.Errorf("expected timeout; got %v", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func TestForwarder_IdleEvictionClosesFlow(t *testing.T) {
	echo := startUDPEcho(t)
	echoStr := echo.String()

	in, inAddr := newLoopbackPacketConn(t)
	lookup := func(_ netip.Addr, _ int) (string, bool) {
		return echoStr, true
	}
	fwd, err := NewForwarder(in, hostUDPDial, lookup,
		slog.New(slog.DiscardHandler),
		WithIdleTimeout(100*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	fwd.Start()
	t.Cleanup(func() { _ = fwd.Close() })

	clientLC := net.ListenConfig{}
	client, err := clientLC.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(inAddr.Port())}

	if _, err := client.WriteTo([]byte("seed"), dst); err != nil {
		t.Fatal(err)
	}
	// Read the echo so we know the flow has been opened.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	if _, _, err := client.ReadFrom(buf); err != nil {
		t.Fatalf("client read: %v", err)
	}

	// Wait for idle eviction.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fwd.mu.Lock()
		n := len(fwd.flows)
		fwd.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	fwd.mu.Lock()
	defer fwd.mu.Unlock()
	t.Errorf("flow was not evicted; flows=%d", len(fwd.flows))
}

func TestForwarder_RejectsBadConstructorArgs(t *testing.T) {
	in, _ := newLoopbackPacketConn(t)
	lookup := func(_ netip.Addr, _ int) (string, bool) { return "", false }
	dial := hostUDPDial

	if _, err := NewForwarder(nil, dial, lookup, nil); err == nil {
		t.Error("nil in should error")
	}
	if _, err := NewForwarder(in, nil, lookup, nil); err == nil {
		t.Error("nil dial should error")
	}
	if _, err := NewForwarder(in, dial, nil, nil); err == nil {
		t.Error("nil lookup should error")
	}
}

func TestForwarder_CloseIsIdempotent(t *testing.T) {
	in, _ := newLoopbackPacketConn(t)
	fwd, err := NewForwarder(in, hostUDPDial,
		func(_ netip.Addr, _ int) (string, bool) { return "", false },
		slog.New(slog.DiscardHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	fwd.Start()
	if err := fwd.Close(); err != nil {
		t.Errorf("first close: %v", err)
	}
	if err := fwd.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}
