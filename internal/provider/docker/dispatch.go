package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hstern/fj-bellows/internal/forgejo"
)

// ExecDispatcher implements orchestrator.Dispatcher by execing into a worker
// container via the docker CLI. It deliberately does not implement
// HostKeyPinner (which is SSH-only); the orchestrator only calls it when the
// dispatcher advertises the capability.
type ExecDispatcher struct {
	runner      cli
	dockerBin   string
	forgejoURL  string
	labels      []string
	waitTimeout time.Duration

	// poll knob, injectable for fast tests.
	pollInterval time.Duration
}

// NewExecDispatcher returns an ExecDispatcher wired against runner. Pass the
// default os/exec-backed runner from NewDefaultRunner in production.
func NewExecDispatcher(runner cli, dockerBin, forgejoURL string, labels []string, waitTimeout time.Duration) *ExecDispatcher {
	if waitTimeout <= 0 {
		waitTimeout = defaultWaitTimeout
	}
	return &ExecDispatcher{
		runner:       runner,
		dockerBin:    dockerBin,
		forgejoURL:   forgejoURL,
		labels:       labels,
		waitTimeout:  waitTimeout,
		pollInterval: 250 * time.Millisecond,
	}
}

// NewDefaultRunner returns the production cli that shells out via os/exec.
func NewDefaultRunner(dockerBin string) cli { //nolint:ireturn // hides the exec impl from main
	if dockerBin == "" {
		dockerBin = defaultDockerBin
	}
	return newExecCLI(dockerBin)
}

// WaitReady polls `docker inspect -f '{{.State.Running}}' <id>` until the
// container reports true, the context is cancelled, or waitTimeout elapses.
// addr is unused: docker exec addresses workers by container ID.
func (d *ExecDispatcher) WaitReady(ctx context.Context, id, _ string) error {
	deadline := time.Now().Add(d.waitTimeout)
	var lastErr error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out, err := d.runner.Run(ctx, nil, "inspect", "-f", "{{.State.Running}}", id)
		if err == nil {
			if strings.TrimSpace(string(out)) == "true" {
				return nil
			}
			lastErr = fmt.Errorf("container %s not running: state=%s", id, strings.TrimSpace(string(out)))
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.pollInterval):
		}
	}
	return fmt.Errorf("container %s not ready within %s: %w", id, d.waitTimeout, lastErr)
}

// RunJob delivers the one-shot registration token to the container via stdin
// (never argv, so it does not appear in the process list) and then execs
// forgejo-runner one-job, blocking until it completes. addr is unused.
func (d *ExecDispatcher) RunJob(ctx context.Context, id, _ string, reg forgejo.Registration, job forgejo.WaitingJob) error {
	// (a) Write the token. `docker exec -i` forwards stdin; sh chmod's it.
	//nolint:gosec // G101: not a credential — this is a shell snippet that reads stdin into /tmp/tok.
	tokenCmd := "cat > /tmp/tok && chmod 600 /tmp/tok"
	if _, err := d.runner.Run(
		ctx, strings.NewReader(reg.Token),
		"exec", "-i", id, "sh", "-c", tokenCmd,
	); err != nil {
		return fmt.Errorf("write token: %w", err)
	}

	// (b) Run one-job. Arguments are passed as a literal argv so the worker's
	// shell never sees them — no shell quoting required.
	args := []string{
		"exec", id,
		"forgejo-runner", "one-job",
		"--url", d.forgejoURL,
		"--uuid", reg.UUID,
		"--token-url", "file:/tmp/tok",
		flagLabel, strings.Join(d.labels, ","),
		"--handle", job.Handle,
		"--wait",
	}
	if _, err := d.runner.Run(ctx, nil, args...); err != nil {
		// A cancelled ctx surfaces as ctx.Err() from execCLI.Run already.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("one-job: %w", err)
	}
	return nil
}
