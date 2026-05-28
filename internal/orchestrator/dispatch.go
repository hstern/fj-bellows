package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hstern/fj-bellows/internal/forgejo"
)

// Dispatcher delivers a single ephemeral job to a worker. It is an interface so
// the orchestrator can be unit-tested without real SSH, and so the dispatch
// mechanism (SSH, docker exec, ...) can be selected per provider. A worker is
// identified by both its provider id and its network addr; SSH dispatch uses
// addr, while a mechanism like docker exec uses id.
type Dispatcher interface {
	// WaitReady blocks until the worker has finished bootstrapping, or the
	// context/timeout expires.
	WaitReady(ctx context.Context, id, addr string) error
	// RunJob delivers the one-shot token to the worker and runs forgejo-runner
	// one-job for the given waiting job, blocking until it completes.
	RunJob(ctx context.Context, id, addr string, reg forgejo.Registration, job forgejo.WaitingJob) error
}

// HostKeyPinner is an optional Dispatcher capability: the orchestrator can
// pre-seed the host key it expects a worker to present, so verification is
// authoritative on the very first dial. A Dispatcher that has no concept of SSH
// host keys (e.g. a future docker-exec dispatcher) simply does not implement it.
type HostKeyPinner interface {
	// PinHostKey records key as the expected SSH host key for the worker at ip.
	PinHostKey(ip string, key ssh.PublicKey)
}

// SSHDispatcher dispatches jobs over SSH using an in-process client.
type SSHDispatcher struct {
	User        string
	Port        int
	Signer      ssh.Signer
	ForgejoURL  string
	Labels      []string
	ReadyFile   string
	ReadyWait   time.Duration // total time to wait for readiness
	DialTimeout time.Duration

	// Log is the optional logger for tunnel warnings (per-conn dial failures
	// etc.). Falls back to slog.Default when nil.
	Log *slog.Logger

	// pinsMu guards pins, the per-VM trust-on-first-use host-key store.
	pinsMu sync.Mutex
	pins   map[string]ssh.PublicKey
}

func (d *SSHDispatcher) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// PinHostKey seeds the pin store with the host key the worker at ip is expected
// to present, under the same addr formula dial uses. A seeded pin makes first
// contact authoritative: tofuHostKeyCallback finds an existing pin on the very
// first dial and requires a byte-equal key instead of trusting and recording
// whatever is presented, which eliminates the trust-on-first-use window in which
// a man-in-the-middle could capture the one-shot token.
func (d *SSHDispatcher) PinHostKey(ip string, key ssh.PublicKey) {
	addr := net.JoinHostPort(ip, strconv.Itoa(d.Port))
	d.pinsMu.Lock()
	defer d.pinsMu.Unlock()
	if d.pins == nil {
		d.pins = make(map[string]ssh.PublicKey)
	}
	d.pins[addr] = key
}

// WaitReady polls SSH until the readiness sentinel exists.
//
//nolint:dupl // intentional shape-match with CacheGatewayDispatcher.WaitReady; the two dispatchers must stay distinct types so only one satisfies HostKeyPinner.
func (d *SSHDispatcher) WaitReady(ctx context.Context, _, addr string) error {
	deadline := time.Now().Add(d.ReadyWait)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		client, err := d.dial(ctx, addr)
		if err != nil {
			lastErr = err
		} else {
			err := runRemote(ctx, client, "test -f "+shellQuote(d.ReadyFile), nil)
			_ = client.Close()
			if err == nil {
				return nil
			}
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("worker %s not ready within %s: %w", addr, d.ReadyWait, lastErr)
}

// RunJob delivers the token and runs one-job to completion.
//
// The dispatch SSH session also carries Forgejo traffic: a reverse port
// forward binds 127.0.0.1:<forgejo-port> on the worker and relays each
// accepted connection to the orchestrator-side Forgejo via the orchestrator's
// own resolver. Combined with a /etc/hosts override on the worker, this lets
// workers on a public cloud reach a LAN-internal Forgejo whose hostname does
// not resolve from the public internet, without any worker-side network
// configuration. TLS SNI is the original hostname, so a public-CA certificate
// for the Forgejo continues to validate end-to-end. See #33.
//
// The runner process is only half the story: each workflow step runs in a
// spawned docker job container with its own network namespace and /etc/hosts.
// To carry the tunnel into those containers we render a forgejo-runner config
// that sets `container.network: host` (so a container's loopback IS the
// worker's loopback, where the tunnel listener sits) and
// `container.options: --add-host=<host>:127.0.0.1` (so the container's DNS
// answer for the Forgejo hostname is 127.0.0.1). Without this every workflow
// using actions/checkout (or any other step that talks back to Forgejo) would
// NXDOMAIN inside its container even though the runner process itself was
// happily on the tunnel. See #37.
func (d *SSHDispatcher) RunJob(ctx context.Context, _, addr string, reg forgejo.Registration, job forgejo.WaitingJob) error {
	target, err := parseForgejoURL(d.ForgejoURL)
	if err != nil {
		return fmt.Errorf("parse forgejo url: %w", err)
	}

	client, err := d.dial(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	tunnel, err := d.startReverseTunnel(ctx, client, target)
	if err != nil {
		return fmt.Errorf("forgejo reverse-tunnel: %w", err)
	}
	defer func() { _ = tunnel.Close() }()

	// Write the one-shot token via stdin so it never appears on the command
	// line / process list.
	if err := runRemote(ctx, client, "cat > /tmp/tok && chmod 600 /tmp/tok", strings.NewReader(reg.Token)); err != nil {
		return fmt.Errorf("write token: %w", err)
	}

	// Write the runner config: docker_host: automount (always), plus tunnel
	// propagation for hostname Forgejo URLs. See runnerConfigYAML.
	if err := runRemote(ctx, client,
		"cat > /tmp/runner-cfg.yml && chmod 600 /tmp/runner-cfg.yml",
		strings.NewReader(runnerConfigYAML(target)),
	); err != nil {
		return fmt.Errorf("write runner config: %w", err)
	}

	cmd := fmt.Sprintf(
		"forgejo-runner one-job --url %s --uuid %s --token-url file:/tmp/tok --label %s --handle %s --wait --config /tmp/runner-cfg.yml",
		shellQuote(d.ForgejoURL),
		shellQuote(reg.UUID),
		shellQuote(strings.Join(d.Labels, ",")),
		shellQuote(job.Handle),
	)
	if prep := hostsOverrideCommand(target); prep != "" {
		cmd = prep + " && " + cmd
	}
	if err := runRemote(ctx, client, cmd, nil); err != nil {
		return fmt.Errorf("one-job: %w", err)
	}
	return nil
}

// runnerConfigYAML returns the forgejo-runner config snippet the dispatcher
// stages at /tmp/runner-cfg.yml and passes via `forgejo-runner one-job
// --config`. Two things go in it:
//
//  1. `docker_host: automount` (always) — forgejo-runner's default `"-"`
//     finds a docker host for the runner process but does NOT mount
//     /var/run/docker.sock into the spawned job containers. Any `docker ...`
//     step then fails with "no such file or directory". automount fixes that.
//     The worker is single-tenant ephemeral; granting the job container
//     root-equivalent access to the host daemon matches every other CI
//     runner stack (GH Actions, GitLab Runner). See #41.
//
//  2. `network: host` + `--add-host=<host>:127.0.0.1` (hostname Forgejo URLs
//     only) — propagates the dispatcher-managed Forgejo tunnel into the
//     spawned job containers, since they have their own network namespace
//     and /etc/hosts. host networking makes the container's loopback the
//     worker's loopback (where the tunnel listener sits); the hosts entry
//     points the Forgejo hostname at it. IP-literal URLs can't be
//     redirected via /etc/hosts (maps names, not IPs) and stay a
//     documented limitation. See #37.
func runnerConfigYAML(t forgejoTarget) string {
	var sb strings.Builder
	sb.WriteString("container:\n")
	sb.WriteString("  docker_host: automount\n")
	if !t.isIPLit {
		sb.WriteString("  network: host\n")
		// YAML double-quoted scalar so a hostname containing special chars
		// (unlikely but cheap to defend) parses safely.
		fmt.Fprintf(&sb, "  options: %q\n", "--add-host="+t.host+":127.0.0.1")
	}
	return sb.String()
}

// forgejoTarget is the parsed Forgejo URL: the hostname or IP literal the
// worker will see in --url, and the effective port (used to bind the worker
// loopback listener and to dial the upstream Forgejo).
type forgejoTarget struct {
	host    string
	port    int
	isIPLit bool
}

// parseForgejoURL extracts {host, port} from the Forgejo URL, defaulting the
// port from scheme when absent. Only http/https are accepted.
func parseForgejoURL(raw string) (forgejoTarget, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return forgejoTarget{}, err
	}
	host := u.Hostname()
	if host == "" {
		return forgejoTarget{}, fmt.Errorf("forgejo url has no host: %q", raw)
	}
	port := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return forgejoTarget{}, fmt.Errorf("forgejo url has invalid port %q", p)
		}
		port = n
	} else {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = 80
		case "https":
			port = 443
		default:
			return forgejoTarget{}, fmt.Errorf("forgejo url has unsupported scheme %q", u.Scheme)
		}
	}
	return forgejoTarget{host: host, port: port, isIPLit: net.ParseIP(host) != nil}, nil
}

// hostsOverrideCommand returns a shell snippet that adds `127.0.0.1 <host>` to
// the worker's /etc/hosts if it isn't already there. Returns "" when no
// override is needed: IP literals don't need DNS, and `localhost` is already
// mapped to 127.0.0.1 on every standard image.
func hostsOverrideCommand(t forgejoTarget) string {
	if t.isIPLit || strings.EqualFold(t.host, "localhost") {
		return ""
	}
	line := "127.0.0.1 " + t.host
	return "grep -qF " + shellQuote(line) + " /etc/hosts || echo " + shellQuote(line) + " >> /etc/hosts"
}

// startReverseTunnel binds 127.0.0.1:port on the worker (via the SSH client's
// global remote-forward channel) and forwards each accepted connection to the
// orchestrator-side Forgejo. The returned io.Closer ends the listener; in-flight
// forwarded connections will close when their underlying client.Close fires.
func (d *SSHDispatcher) startReverseTunnel(ctx context.Context, client *ssh.Client, target forgejoTarget) (io.Closer, error) {
	bind := net.JoinHostPort("127.0.0.1", strconv.Itoa(target.port))
	listener, err := client.Listen("tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("remote-forward %s: %w", bind, err)
	}
	go d.acceptLoop(ctx, listener, target)
	return listener, nil
}

// acceptLoop accepts connections from the worker and hands each off to a
// per-conn forward goroutine. It exits when listener.Accept returns an error
// (typically because the listener was closed during defer in RunJob).
func (d *SSHDispatcher) acceptLoop(ctx context.Context, listener net.Listener, target forgejoTarget) {
	for {
		workerConn, err := listener.Accept()
		if err != nil {
			return
		}
		go d.forwardOne(ctx, workerConn, target)
	}
}

// forwardOne dials the orchestrator-side Forgejo and bridges bytes both ways
// until either side closes. A dial failure logs and drops just this one conn,
// leaving the listener up so subsequent connections can still try.
func (d *SSHDispatcher) forwardOne(ctx context.Context, workerConn net.Conn, target forgejoTarget) {
	defer func() { _ = workerConn.Close() }()
	dialer := net.Dialer{Timeout: 10 * time.Second}
	upstream, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(target.host, strconv.Itoa(target.port)))
	if err != nil {
		d.logger().Warn("forgejo tunnel: dial upstream", "host", target.host, "port", target.port, "err", err)
		return
	}
	defer func() { _ = upstream.Close() }()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(upstream, workerConn) }()
	go func() { defer wg.Done(); _, _ = io.Copy(workerConn, upstream) }()
	wg.Wait()
}

func (d *SSHDispatcher) dial(ctx context.Context, ip string) (*ssh.Client, error) {
	addr := net.JoinHostPort(ip, strconv.Itoa(d.Port))
	cfg := &ssh.ClientConfig{
		User:            d.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(d.Signer)},
		HostKeyCallback: d.tofuHostKeyCallback(addr),
		Timeout:         d.DialTimeout,
	}
	dialer := net.Dialer{Timeout: d.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
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

func runRemote(ctx context.Context, client *ssh.Client, cmd string, stdin io.Reader) error {
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()
	if stdin != nil {
		sess.Stdin = stdin
	}
	// Closing the session unblocks CombinedOutput, so a cancelled context
	// interrupts even a long-running `one-job --wait` instead of leaking the
	// dispatch goroutine. The watcher exits via done when the command returns.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-done:
		}
	}()
	out, err := sess.CombinedOutput(cmd)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// shellQuote single-quotes a string for safe use in a remote shell command.
// Inside single quotes every byte is literal except `'`, which is escaped as
// '\” — so even attacker-influenced values (job handle, uuid, labels) cannot
// break out of the quoting.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tofuHostKeyCallback returns a trust-on-first-use (TOFU) host-key verification
// policy for the worker VM at addr.
//
// SECURITY: workers are created fresh per billing hour, so their host key is not
// known in advance and cannot be pre-pinned. Instead we pin per-VM on first
// contact: the first successful handshake to addr records the presented host
// key in the dispatcher's pin store; every later dial to the same addr requires
// a byte-equal key and rejects any mismatch (a possible man-in-the-middle that
// appeared after first contact). Different addrs are pinned independently.
//
// Residual risk: a man-in-the-middle present at the very first contact with a
// VM could still impersonate it and capture the one-shot ephemeral runner
// token. After that first connect, the VM's identity is verified for the
// remainder of its life.
//
// That residual risk is eliminated when the pin is seeded ahead of time via
// PinHostKey: the callback then finds an existing pin on the first dial and
// verifies against it rather than recording the presented key, so even first
// contact is authoritative. The orchestrator seeds such a pin after generating
// the worker's host key and injecting its private half via cloud-init.
//
//nolint:dupl // intentional shape-match with CacheGatewayDispatcher.tofuHostKeyCallback; the two dispatchers must stay distinct types so only one satisfies HostKeyPinner.
func (d *SSHDispatcher) tofuHostKeyCallback(addr string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		d.pinsMu.Lock()
		defer d.pinsMu.Unlock()
		if d.pins == nil {
			d.pins = make(map[string]ssh.PublicKey)
		}
		pinned, ok := d.pins[addr]
		if !ok {
			// First contact: trust and record the presented key.
			d.pins[addr] = key
			return nil
		}
		if !bytes.Equal(pinned.Marshal(), key.Marshal()) {
			return fmt.Errorf(
				"host key mismatch for %s: presented %s key does not match pinned key (possible MITM)",
				addr, key.Type(),
			)
		}
		return nil
	}
}
