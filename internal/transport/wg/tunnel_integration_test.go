//go:build wgintegration

package wg

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// TestTwoTunnelsTalk wires two in-process Tunnels to each other on
// loopback UDP and exchanges TCP bytes through netstack. Validates the
// netstack wiring end-to-end without needing real cache + worker.
//
// Build-tag gated (`wgintegration`) so it doesn't run by default — the
// handshake takes seconds and the test reserves two ephemeral UDP
// ports. Run explicitly with:
//
//	go test -tags wgintegration ./internal/transport/wg/
func TestTwoTunnelsTalk(t *testing.T) {
	portA := freeUDPPort(t)
	portB := freeUDPPort(t)

	privA, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	privB, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	addrA := netip.MustParseAddr("10.99.0.1")
	addrB := netip.MustParseAddr("10.99.0.2")

	tA, err := New(Config{
		PrivateKey:        privA,
		LocalAddr:         addrA,
		ListenPort:        portA,
		KeepaliveInterval: 500 * time.Millisecond,
		Peer: Peer{
			PublicKey:  privB.PublicKey(),
			Endpoint:   &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: portB},
			AllowedIPs: []netip.Prefix{netip.PrefixFrom(addrB, 32)},
		},
	})
	if err != nil {
		t.Fatalf("Tunnel A: %v", err)
	}
	defer tA.Close()

	tB, err := New(Config{
		PrivateKey:        privB,
		LocalAddr:         addrB,
		ListenPort:        portB,
		KeepaliveInterval: 500 * time.Millisecond,
		Peer: Peer{
			PublicKey:  privA.PublicKey(),
			Endpoint:   &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: portA},
			AllowedIPs: []netip.Prefix{netip.PrefixFrom(addrA, 32)},
		},
	})
	if err != nil {
		t.Fatalf("Tunnel B: %v", err)
	}
	defer tB.Close()

	// B accepts on its own tunnel-side address; A dials through.
	listener, err := tB.ListenTCP(&net.TCPAddr{IP: addrB.AsSlice(), Port: 8443})
	if err != nil {
		t.Fatalf("ListenTCP on B: %v", err)
	}
	defer listener.Close()

	// Echo server in B's netstack.
	srvErr := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer c.Close()
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil {
			srvErr <- err
			return
		}
		if _, err := c.Write(buf[:n]); err != nil {
			srvErr <- err
			return
		}
		srvErr <- nil
	}()

	// A dials B's address through its own tunnel netstack.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var dialErr error
	var client net.Conn
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		client, dialErr = tA.DialContext(ctx, "tcp", net.JoinHostPort(addrB.String(), "8443"))
		if dialErr == nil {
			break
		}
		// First handshake takes one keepalive interval — retry briefly.
		time.Sleep(250 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("dial through tunnel A → B: %v", dialErr)
	}
	defer client.Close()

	const payload = "ping over WG"
	if _, err := client.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != payload {
		t.Errorf("echo mismatch: got %q want %q", got, payload)
	}
	if err := <-srvErr; err != nil {
		t.Errorf("echo server: %v", err)
	}
}

// freeUDPPort grabs an OS-assigned ephemeral UDP port and immediately
// closes it. There's a small race window before WG's bind reclaims it
// but that's the standard pattern for ephemeral-port acquisition.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port
}
