package wg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultProxyDialTimeout bounds how long the upstream dial may block
// per connection. Sensible default for HTTPS / git-over-ssh — both
// complete TCP handshake well under 10s on a working LAN; anything
// slower than that is likely an upstream-down situation we'd rather
// surface fast than queue connections behind.
const DefaultProxyDialTimeout = 10 * time.Second

// ProxySpec describes one accept-and-bridge instance. The orchestrator
// builds a slice of these from config.Transport.WG.Proxies by:
//   - calling Tunnel.ListenTCP for each Listen
//   - pairing each listener with its Upstream
//   - passing the slice to NewProxy.
//
// Keeping the listener pre-made (rather than taking a tunnel + addresses
// here) lets tests drive the proxy with plain loopback net.Listeners.
type ProxySpec struct {
	// Listener accepts incoming connections (tunnel-side under normal
	// operation; loopback in tests). Proxy takes ownership and Closes
	// it during shutdown.
	Listener net.Listener

	// Upstream is the "host:port" dialed for each accepted connection,
	// over the orchestrator host's normal network. The proxy never
	// reads payload bytes — TLS terminates end-to-end with the real
	// upstream, this is a dumb byte-bridge.
	Upstream string

	// DialTimeout bounds the per-connection upstream dial. Zero =
	// DefaultProxyDialTimeout (10s).
	DialTimeout time.Duration
}

// Proxy is a fan-out of transparent TCP bridges. It owns one accept-
// loop goroutine per ProxySpec and one pair of io.Copy goroutines per
// accepted connection. Close stops the accept loops and waits for
// every bridge goroutine to finish before returning.
type Proxy struct {
	specs []ProxySpec
	log   *slog.Logger

	// wg covers ALL goroutines spawned by the proxy: accept loops
	// AND per-connection bridges. Close waits on it.
	wg sync.WaitGroup

	// connSeq numbers each accepted connection for log correlation.
	connSeq atomic.Uint64

	mu      sync.Mutex
	closed  bool
	conns   map[net.Conn]struct{} // in-flight connections for force-close on shutdown
	upConns map[net.Conn]struct{} // matching upstream conns
}

// NewProxy returns a Proxy ready to be Started. specs must be non-empty
// and each spec's Listener + Upstream non-zero; NewProxy validates
// before doing any work so config errors surface synchronously.
func NewProxy(specs []ProxySpec, log *slog.Logger) (*Proxy, error) {
	if len(specs) == 0 {
		return nil, errors.New("wg/proxy: at least one ProxySpec required")
	}
	for i, s := range specs {
		if s.Listener == nil {
			return nil, fmt.Errorf("wg/proxy: specs[%d].Listener is nil", i)
		}
		if s.Upstream == "" {
			return nil, fmt.Errorf("wg/proxy: specs[%d].Upstream is empty", i)
		}
		if _, _, err := net.SplitHostPort(s.Upstream); err != nil {
			return nil, fmt.Errorf("wg/proxy: specs[%d].Upstream %q is not host:port: %w", i, s.Upstream, err)
		}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Proxy{
		specs:   specs,
		log:     log,
		conns:   make(map[net.Conn]struct{}),
		upConns: make(map[net.Conn]struct{}),
	}, nil
}

// Start spawns one accept-loop goroutine per ProxySpec. Returns
// immediately; the proxy runs until Close is called. Safe to call
// exactly once.
func (p *Proxy) Start() {
	for _, spec := range p.specs {
		p.wg.Add(1)
		go p.acceptLoop(spec)
	}
}

// Close stops accepting, force-closes in-flight bridges, and waits for
// every goroutine to finish before returning. Idempotent.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	// Snapshot listeners so we can close outside the lock.
	specs := p.specs
	conns := make([]net.Conn, 0, len(p.conns)+len(p.upConns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	for c := range p.upConns {
		conns = append(conns, c)
	}
	p.mu.Unlock()

	for _, s := range specs {
		_ = s.Listener.Close()
	}
	// Force-close in-flight bridges so io.Copy unblocks.
	for _, c := range conns {
		_ = c.Close()
	}
	p.wg.Wait()
	return nil
}

func (p *Proxy) acceptLoop(spec ProxySpec) {
	defer p.wg.Done()
	dialTimeout := spec.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = DefaultProxyDialTimeout
	}
	for {
		client, err := spec.Listener.Accept()
		if err != nil {
			if p.isClosed() {
				return
			}
			// Some Accept errors are recoverable (e.g. EAGAIN under
			// load) — net.Listener implementations generally re-poll
			// internally. But persistent failures (closed listener,
			// permission errors) are terminal. Log + exit; the proxy
			// stays partially up if other specs are still running.
			p.log.Error("wg/proxy: accept failed; stopping listener", "upstream", spec.Upstream, "err", err)
			return
		}
		p.wg.Add(1)
		go p.handle(client, spec.Upstream, dialTimeout)
	}
}

func (p *Proxy) handle(client net.Conn, upstream string, dialTimeout time.Duration) {
	defer p.wg.Done()
	id := p.connSeq.Add(1)
	clientAddr := client.RemoteAddr().String()
	logger := p.log.With("conn_id", id, "client", clientAddr, "upstream", upstream)

	if !p.trackClient(client) {
		_ = client.Close()
		return
	}
	defer func() {
		p.untrackClient(client)
		_ = client.Close()
	}()

	dialCtx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	dialer := net.Dialer{}
	up, err := dialer.DialContext(dialCtx, "tcp", upstream)
	cancel()
	if err != nil {
		logger.Warn("wg/proxy: upstream dial failed", "err", err)
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

	logger.Debug("wg/proxy: connection bridged")
	startedAt := time.Now()
	clientToUpstream, upstreamToClient := bridge(client, up)
	logger.Debug(
		"wg/proxy: connection closed",
		"bytes_client_to_upstream", clientToUpstream,
		"bytes_upstream_to_client", upstreamToClient,
		"dur", time.Since(startedAt).String(),
	)
}

// bridge copies bytes both ways between client and upstream until both
// directions complete. When one direction's Copy returns, the other
// side gets CloseWrite (when supported) so the FIN propagates promptly
// instead of waiting on dead-conn timeouts. Returns the per-direction
// byte counts (client→upstream, upstream→client).
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

// closeWrite signals "no more bytes from this direction" on conns that
// support half-close. TCP conns (net.TCPConn, gVisor netstack TCP
// endpoints) implement it; many fake/wrapped conns don't — the type
// assertion silently no-ops for those, which is fine.
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

// trackClient registers an accepted client connection for force-close
// on shutdown. Returns false (and the caller should close + return)
// when the proxy has already been Closed; this races shutdown vs. a
// late Accept return.
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
