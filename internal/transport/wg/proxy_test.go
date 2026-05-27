package wg

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
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

// startProxy wires up a loopback listener + upstream + Proxy, returns
// the listen address client should dial. Cleanup torn down by t.Cleanup.
func startProxy(t *testing.T, upstream string) string {
	t.Helper()
	lc := net.ListenConfig{}
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	specs := []ProxySpec{{Listener: ln, Upstream: upstream}}
	p, err := NewProxy(specs, slog.New(slog.DiscardHandler))
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	p.Start()
	t.Cleanup(func() { _ = p.Close() })
	return ln.Addr().String()
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

func TestProxy_BridgesByteStream(t *testing.T) {
	upstreamAddr, closeUpstream := startUpstream(t, echoHandler)
	defer closeUpstream()

	proxyAddr := startProxy(t, upstreamAddr)

	c := dialLoopback(t, proxyAddr)
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

// Half-close: client CloseWrite → upstream sees EOF on its Read →
// proxy's io.Copy returns → closeWrite propagates to the other side →
// upstream's write side is closed → both directions complete.
func TestProxy_HalfClose(t *testing.T) {
	upstreamAddr, closeUpstream := startUpstream(t, func(c net.Conn) {
		b, err := io.ReadAll(c)
		if err != nil {
			return
		}
		_, _ = c.Write(append(b, []byte(" + tail")...))
	})
	defer closeUpstream()

	proxyAddr := startProxy(t, upstreamAddr)
	c := dialLoopback(t, proxyAddr)
	defer func() { _ = c.Close() }()

	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	tc, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatal("expected *net.TCPConn")
	}
	if err := tc.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read after half-close: %v", err)
	}
	if string(got) != "ping + tail" {
		t.Errorf("got %q, want %q", got, "ping + tail")
	}
}

// Upstream unreachable → proxy's accept loop dials and fails; client
// gets a closed conn promptly. No hangs.
func TestProxy_UpstreamUnreachable(t *testing.T) {
	proxyAddr := startProxy(t, "127.0.0.1:1")

	c := dialLoopback(t, proxyAddr)
	defer func() { _ = c.Close() }()

	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 8)
	n, err := c.Read(buf)
	if n != 0 {
		t.Errorf("expected 0 bytes read; got %d (%q)", n, buf[:n])
	}
	if err == nil {
		t.Error("expected EOF / connection-reset; got nil")
	}
}

func TestProxy_ValidatesSpecs(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if _, err := NewProxy(nil, log); err == nil {
		t.Fatal("empty specs should error")
	}
	lc := net.ListenConfig{}
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	cases := []struct {
		name    string
		spec    ProxySpec
		wantSub string
	}{
		{"nil listener", ProxySpec{Upstream: "x:1"}, "Listener is nil"},
		{"empty upstream", ProxySpec{Listener: ln}, "Upstream is empty"},
		{"bad upstream shape", ProxySpec{Listener: ln, Upstream: "no-port"}, "is not host:port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewProxy([]ProxySpec{tc.spec}, log)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestProxy_CloseIdempotentAndDrains(t *testing.T) {
	upstreamAddr, closeUpstream := startUpstream(t, echoHandler)
	defer closeUpstream()

	lc := net.ListenConfig{}
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProxy([]ProxySpec{{Listener: ln, Upstream: upstreamAddr}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	p.Start()

	// Open a connection; let it idle in the bridge.
	c := dialLoopback(t, ln.Addr().String())
	defer func() { _ = c.Close() }()

	// Close the proxy. Must drain (force-close the in-flight bridge)
	// and return without hanging.
	done := make(chan error, 1)
	go func() { done <- p.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung; bridge was not force-closed")
	}

	// Second Close is a no-op.
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// Post-Close dials are rejected because the listener is closed.
	d := net.Dialer{}
	dialCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := d.DialContext(dialCtx, "tcp", ln.Addr().String()); err == nil {
		t.Error("dial after Close should error (listener closed)")
	} else if !errors.Is(err, errConnRefused()) && !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "closed") {
		t.Logf("post-Close dial error (informational): %v", err)
	}
}

// errConnRefused returns a sentinel for the platform-specific
// connection-refused error so the test above can use errors.Is.
// net.Errorf("connection refused") doesn't have a stable Is target;
// just use the string match in the test itself.
func errConnRefused() error { return errors.New("connection refused") }
