package orchestrator

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
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
		HostKeyCallback: hostKeyCallback(),
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

// hostKeyCallback returns the SSH host-key verification policy for worker VMs.
//
// SECURITY (known limitation): workers are created fresh per billing hour and
// their host key is not known in advance, so it cannot be pre-pinned. We
// therefore do not verify the host key. The channel is still encrypted and the
// VM authenticates us via the public key injected at provision time, but a
// man-in-the-middle on the path to the worker's public IP could impersonate the
// VM and capture the one-shot ephemeral runner token. Hardening path: inject a
// known host key via cloud-init, or pin per-VM on first connect (TOFU). Tracked
// as a known limitation in README.md / AGENTS.md.
func hostKeyCallback() ssh.HostKeyCallback {
	//nolint:gosec // G106: documented known limitation — fresh per-hour VMs have no pre-known host key. See doc above.
	return ssh.InsecureIgnoreHostKey()
}
