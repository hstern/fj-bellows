package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hstern/fj-bellows/internal/forgejo"
)

// CacheGatewayDispatcher dispatches jobs over SSH to a worker reachable
// via its VPC address through an IPsec tunnel (FJB-64). Unlike
// SSHDispatcher, it deliberately does NOT implement HostKeyPinner —
// the orchestrator's host-key seeding logic auto-skips because the
// type-assertion fails. This drops cloud-init's host-key injection on
// the worker and the per-VM ed25519 generation in the orchestrator.
//
// Workers reach LAN services (git.stern.ca, etc.) via the cache
// nanode's DNS resolver + IPsec routing — there is no reverse port-
// forward on the dispatch session and no /etc/hosts rewrite at
// dispatch time. The runner config emits only `docker_host: automount`
// (the dispatch-time hostname-redirect glue from the SSH-tunnel model
// is unnecessary).
//
// Host-key verification uses trust-on-first-use (TOFU) — the IPsec
// tunnel authenticates the network path and Linode VPC isolation
// prevents spoofing within the VPC, so MitM at the first dial would
// require compromising the IPsec endpoint or the VPC itself. TOFU
// provides cheap post-first-contact integrity beyond those guarantees.
type CacheGatewayDispatcher struct {
	User        string
	Port        int
	Signer      ssh.Signer
	ForgejoURL  string
	Labels      []string
	ReadyFile   string
	ReadyWait   time.Duration
	DialTimeout time.Duration

	// DialFn is the network dialer the dispatcher uses to reach worker
	// VPC IPs. Under FJB-92 the orchestrator wires this to the
	// embedded WG tunnel's netstack DialContext so packets to 10.0.0.X
	// are encapsulated and routed via the cache nanode, instead of
	// hitting the host's kernel route table (where there is no route to
	// the worker VPC). Nil falls back to a plain net.Dialer so unit
	// tests that don't care about routing keep working; cmd/fj-bellows
	// always sets it in production.
	DialFn func(ctx context.Context, network, addr string) (net.Conn, error)

	// Log is currently unused — there are no per-conn warnings to
	// emit (no reverse tunnel, no /etc/hosts mutation). Reserved for
	// future diagnostics so callers can pre-wire a logger.
	Log *slog.Logger

	pinsMu sync.Mutex
	pins   map[string]ssh.PublicKey
}

// WaitReady polls SSH on the worker's VPC address until the readiness
// sentinel exists, or ReadyWait elapses.
//
//nolint:dupl // intentional shape-match with SSHDispatcher.WaitReady; the two dispatchers must stay distinct types so only one satisfies HostKeyPinner.
func (d *CacheGatewayDispatcher) WaitReady(ctx context.Context, _, addr string) error {
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

// RunJob delivers the one-shot Forgejo runner token to the worker via
// stdin and runs `forgejo-runner one-job --wait`. Unlike the SSH-
// tunnel model there is no reverse port-forward and no /etc/hosts
// rewrite: workers reach the Forgejo URL via the cache nanode's DNS
// resolver + IPsec routing.
func (d *CacheGatewayDispatcher) RunJob(ctx context.Context, _, addr string, reg forgejo.Registration, job forgejo.WaitingJob) error {
	client, err := d.dial(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// Token via stdin so it never appears on a process listing.
	if err := runRemote(ctx, client, "cat > /tmp/tok && chmod 600 /tmp/tok", strings.NewReader(reg.Token)); err != nil {
		return fmt.Errorf("write token: %w", err)
	}

	// Minimal runner config: automount docker.sock into the spawned
	// job containers. No `--add-host` hostname-redirect plumbing — the
	// container's resolver hits unbound (via the worker's host-network
	// resolver) and reaches the Forgejo URL via the cache's DNS path.
	if err := runRemote(ctx, client,
		"cat > /tmp/runner-cfg.yml && chmod 600 /tmp/runner-cfg.yml",
		strings.NewReader("container:\n  docker_host: automount\n"),
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
	if err := runRemote(ctx, client, cmd, nil); err != nil {
		return fmt.Errorf("one-job: %w", err)
	}
	return nil
}

// dial opens an SSH connection to the worker at the given VPC address.
// TOFU host-key policy: the first successful handshake records the key;
// subsequent dials reject any mismatch.
func (d *CacheGatewayDispatcher) dial(ctx context.Context, addr string) (*ssh.Client, error) {
	target := net.JoinHostPort(addr, strconv.Itoa(d.Port))
	cfg := &ssh.ClientConfig{
		User:            d.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(d.Signer)},
		HostKeyCallback: d.tofuHostKeyCallback(target),
		Timeout:         d.DialTimeout,
	}
	dialCtx := ctx
	if d.DialTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, d.DialTimeout)
		defer cancel()
	}
	conn, err := d.dialNet(dialCtx, "tcp", target)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", target, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, target, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", target, err)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// dialNet is the net-layer dial used by dial(). It prefers an injected
// DialFn (the WG tunnel's netstack DialContext under FJB-92) so worker
// VPC IPs are reachable; falls back to a plain net.Dialer for tests
// that don't wire one.
func (d *CacheGatewayDispatcher) dialNet(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.DialFn != nil {
		return d.DialFn(ctx, network, addr)
	}
	dialer := net.Dialer{Timeout: d.DialTimeout}
	return dialer.DialContext(ctx, network, addr)
}

//nolint:dupl // intentional shape-match with SSHDispatcher.tofuHostKeyCallback; the two dispatchers must stay distinct types so only one satisfies HostKeyPinner.
func (d *CacheGatewayDispatcher) tofuHostKeyCallback(addr string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		d.pinsMu.Lock()
		defer d.pinsMu.Unlock()
		if d.pins == nil {
			d.pins = make(map[string]ssh.PublicKey)
		}
		pinned, ok := d.pins[addr]
		if !ok {
			d.pins[addr] = key
			return nil
		}
		if !bytes.Equal(pinned.Marshal(), key.Marshal()) {
			return fmt.Errorf(
				"host key mismatch for %s: presented %s key does not match TOFU-pinned key (possible MITM inside the IPsec tunnel or VPC)",
				addr, key.Type(),
			)
		}
		return nil
	}
}
