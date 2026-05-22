package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hstern/fj-bellows/internal/forgejo"
)

// Dispatcher delivers a single ephemeral job to a worker VM. It is an interface
// so the orchestrator can be unit-tested without real SSH.
type Dispatcher interface {
	// WaitReady blocks until the worker at ip has finished cloud-init, or the
	// context/timeout expires.
	WaitReady(ctx context.Context, ip string) error
	// RunJob writes the one-shot token to the worker and runs forgejo-runner
	// one-job for the given waiting job, blocking until it completes.
	RunJob(ctx context.Context, ip string, reg forgejo.Registration, job forgejo.WaitingJob) error
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

	// pinsMu guards pins, the per-VM trust-on-first-use host-key store.
	pinsMu sync.Mutex
	pins   map[string]ssh.PublicKey
}

// WaitReady polls SSH until the readiness sentinel exists.
func (d *SSHDispatcher) WaitReady(ctx context.Context, ip string) error {
	deadline := time.Now().Add(d.ReadyWait)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		client, err := d.dial(ctx, ip)
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
	return fmt.Errorf("worker %s not ready within %s: %w", ip, d.ReadyWait, lastErr)
}

// RunJob delivers the token and runs one-job to completion.
func (d *SSHDispatcher) RunJob(ctx context.Context, ip string, reg forgejo.Registration, job forgejo.WaitingJob) error {
	client, err := d.dial(ctx, ip)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// Write the one-shot token via stdin so it never appears on the command
	// line / process list.
	if err := runRemote(ctx, client, "cat > /tmp/tok && chmod 600 /tmp/tok", strings.NewReader(reg.Token)); err != nil {
		return fmt.Errorf("write token: %w", err)
	}

	cmd := fmt.Sprintf(
		"forgejo-runner one-job --url %s --uuid %s --token-url file:/tmp/tok --label %s --handle %s --wait",
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
// remainder of its life. Eliminating the residual risk would require injecting
// a known host key via cloud-init (out of scope here).
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
