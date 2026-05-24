package linode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// cacheTunnel maintains a persistent ssh -R from the orchestrator to
// the cache VM, binding 127.0.0.1:<upstream-port> on the cache VM and
// bridging each accepted connection to upstream.url from the
// orchestrator's network namespace.
//
// Why this exists (FJB-7): the cache VM is provisioned at Configure-
// time and never participates in a dispatch SSH session, so it never
// inherits the per-dispatch worker tunnel that workers use to reach a
// LAN-internal Forgejo. Its only paths to upstream are its public NIC
// (cannot reach RFC1918) and its VPC NIC (visible only inside the same
// VPC). A long-lived reverse-tunnel from the orchestrator gives the
// cache the same LAN reach the orchestrator has.
//
// Pairs with the cache cloud-init's /etc/hosts override (which points
// the upstream hostname at 127.0.0.1 on the cache VM) so zot's sync
// extension dials the tunneled loopback transparently. TLS SNI stays
// the original hostname, so the operator's public-CA cert continues to
// validate end-to-end.
//
// Lifecycle matches the managedFirewall refresh loop: context.Background-
// rooted goroutine (no Provider.Shutdown hook), explicit Stop()
// invoked by maybeCleanupCache before DeleteInstance so the loop
// doesn't churn against a destroyed VM.
type cacheTunnel struct {
	signer  ssh.Signer
	sshUser string
	sshPort int

	upstreamHost string
	upstreamPort int

	// lookupIP resolves the cache VM's public IPv4 at each connect
	// attempt. Injected so tests don't need the linodego client; the
	// production wiring passes a closure that re-reads from
	// managedCache's cached IP (populated at create) and falls back to
	// ListInstances when the cache field is empty.
	lookupIP func(context.Context) (string, error)

	log *slog.Logger

	// initialBackoff is the first sleep after a failed connect; the
	// loop doubles it up to maxBackoff. 0 = use defaultInitialBackoff.
	initialBackoff time.Duration

	// maxBackoff caps the per-iteration wait. 0 = defaultMaxBackoff.
	maxBackoff time.Duration

	// dialTimeout bounds each TCP+SSH handshake attempt. 0 = default.
	dialTimeout time.Duration

	// dialTCP is a seam for tests: production leaves it nil and the
	// loop uses net.Dialer.DialContext. Tests substitute a function
	// that returns an in-memory net.Pipe end.
	dialTCP func(ctx context.Context, network, addr string) (net.Conn, error)

	// dialUpstream is the dialer used to reach the actual upstream
	// from the orchestrator side. Production leaves it nil and the
	// loop uses net.Dialer. Tests substitute to assert reachability
	// without a real server.
	dialUpstream func(ctx context.Context, network, addr string) (net.Conn, error)

	// stop is closed by Stop() to signal the loop to exit. done is
	// closed by the loop when it has fully drained. Both are nil
	// until Start() is called; we deliberately do NOT store a
	// context.Context on the struct (the containedctx linter is
	// right that long-lived goroutines should derive contexts per
	// iteration rather than holding one across the struct lifetime).
	stop chan struct{}
	done chan struct{}

	// stopOnce guards Stop's close(stop) so repeat calls are no-ops.
	stopOnce sync.Once

	// pinsMu guards pins, the TOFU host-key store keyed by addr.
	// Reused across reconnects so a key-rotation on the cache VM
	// surfaces as a hard error (not silent re-trust).
	pinsMu sync.Mutex
	pins   map[string]ssh.PublicKey
}

const (
	defaultCacheTunnelInitialBackoff = time.Second
	defaultCacheTunnelMaxBackoff     = 30 * time.Second
	defaultCacheTunnelDialTimeout    = 15 * time.Second
)

// Start spawns the reconnect loop. Safe to call exactly once per
// cacheTunnel; subsequent calls are no-ops.
func (t *cacheTunnel) Start() {
	if t.done != nil {
		return
	}
	if t.log == nil {
		t.log = slog.Default()
	}
	if t.initialBackoff == 0 {
		t.initialBackoff = defaultCacheTunnelInitialBackoff
	}
	if t.maxBackoff == 0 {
		t.maxBackoff = defaultCacheTunnelMaxBackoff
	}
	if t.dialTimeout == 0 {
		t.dialTimeout = defaultCacheTunnelDialTimeout
	}
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	go t.loop()
}

// Stop cancels the loop and waits for it to drain. Safe to call when
// Start was never invoked or already stopped.
func (t *cacheTunnel) Stop() {
	if t.stop == nil {
		return
	}
	t.stopOnce.Do(func() { close(t.stop) })
	<-t.done
}

// loopCtx returns a context that is cancelled when Stop is called.
// Deriving it here (rather than storing one on the struct) keeps the
// long-lived goroutine free of a stored ctx — see containedctx — and
// gives each runOnce a fresh derived context.
func (t *cacheTunnel) loopCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-t.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (t *cacheTunnel) loop() {
	defer close(t.done)
	backoff := t.initialBackoff
	for {
		select {
		case <-t.stop:
			return
		default:
		}
		ctx, cancel := t.loopCtx()
		err := t.runOnce(ctx)
		cancel()
		switch {
		case err == nil:
			// Clean shutdown via Stop().
			return
		case errors.Is(err, context.Canceled):
			return
		default:
			t.log.Warn("cache tunnel: session ended", "err", err, "retry_in", backoff)
		}
		select {
		case <-t.stop:
			return
		case <-time.After(backoff):
		}
		if backoff < t.maxBackoff {
			backoff *= 2
			if backoff > t.maxBackoff {
				backoff = t.maxBackoff
			}
		}
	}
}

// runOnce performs one full cycle: look up the cache VM's IP, dial
// SSH, open the reverse listener, accept forever (or until the SSH
// client disconnects). Returns nil only on clean ctx cancellation.
func (t *cacheTunnel) runOnce(ctx context.Context) error {
	ip, err := t.lookupIP(ctx)
	if err != nil {
		return fmt.Errorf("cache vm ip: %w", err)
	}
	if ip == "" {
		return errors.New("cache vm public ip not yet assigned")
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(t.sshPort))

	client, err := t.dial(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	bind := net.JoinHostPort("127.0.0.1", strconv.Itoa(t.upstreamPort))
	listener, err := client.Listen("tcp", bind)
	if err != nil {
		return fmt.Errorf("remote-forward %s: %w", bind, err)
	}
	defer func() { _ = listener.Close() }()

	t.log.Info("cache tunnel: established",
		"cache_addr", addr, "bind", bind,
		"upstream_host", t.upstreamHost, "upstream_port", t.upstreamPort,
	)

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			workerConn, err := listener.Accept()
			if err != nil {
				return
			}
			go t.forward(ctx, workerConn)
		}
	}()

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		_ = client.Wait()
	}()

	select {
	case <-ctx.Done():
		return nil
	case <-clientDone:
		return errors.New("ssh client disconnected")
	case <-acceptDone:
		return errors.New("reverse listener closed")
	}
}

// dial performs the TCP + SSH handshake against the cache VM, honoring
// the dispatcher-style TOFU host-key policy and the tests' optional
// dialTCP override.
func (t *cacheTunnel) dial(ctx context.Context, addr string) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            t.sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(t.signer)},
		HostKeyCallback: t.tofuHostKeyCallback(addr),
		Timeout:         t.dialTimeout,
	}
	dctx, cancel := context.WithTimeout(ctx, t.dialTimeout)
	defer cancel()
	conn, err := t.tcpDial(dctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

func (t *cacheTunnel) tcpDial(ctx context.Context, network, addr string) (net.Conn, error) {
	if t.dialTCP != nil {
		return t.dialTCP(ctx, network, addr)
	}
	d := net.Dialer{Timeout: t.dialTimeout}
	return d.DialContext(ctx, network, addr)
}

// forward bridges a single cache→orchestrator stream to upstream. The
// dialer is the orchestrator-side net.Dialer (or the test override),
// so RFC1918 / split-horizon DNS reach the upstream from the
// orchestrator's network namespace — exactly what the cache VM cannot
// do on its own.
func (t *cacheTunnel) forward(ctx context.Context, cacheConn net.Conn) {
	defer func() { _ = cacheConn.Close() }()
	upstreamAddr := net.JoinHostPort(t.upstreamHost, strconv.Itoa(t.upstreamPort))
	upstream, err := t.upstreamDial(ctx, upstreamAddr)
	if err != nil {
		t.log.Warn("cache tunnel: dial upstream",
			"host", t.upstreamHost, "port", t.upstreamPort, "err", err,
		)
		return
	}
	defer func() { _ = upstream.Close() }()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(upstream, cacheConn) }()
	go func() { defer wg.Done(); _, _ = io.Copy(cacheConn, upstream) }()
	wg.Wait()
}

func (t *cacheTunnel) upstreamDial(ctx context.Context, addr string) (net.Conn, error) {
	if t.dialUpstream != nil {
		return t.dialUpstream(ctx, "tcp", addr)
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

// tofuHostKeyCallback is the cache-tunnel-local sibling of the
// dispatcher's: trust-on-first-use per addr, hard-reject mismatches on
// subsequent dials. The cache VM lives for the whole deployment so
// host-key rotation should be rare; if it happens (e.g. operator
// reseeds), the operator is expected to clear the pin via daemon
// restart.
func (t *cacheTunnel) tofuHostKeyCallback(addr string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		t.pinsMu.Lock()
		defer t.pinsMu.Unlock()
		if t.pins == nil {
			t.pins = make(map[string]ssh.PublicKey)
		}
		pinned, ok := t.pins[addr]
		if !ok {
			t.pins[addr] = key
			return nil
		}
		if !bytes.Equal(pinned.Marshal(), key.Marshal()) {
			return fmt.Errorf(
				"cache tunnel host key mismatch for %s: presented %s key does not match pinned key",
				addr, key.Type(),
			)
		}
		return nil
	}
}
