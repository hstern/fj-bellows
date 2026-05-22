package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/hstern/fj-bellows/internal/forgejo"
)

// tokenPath is where the one-shot registration token is written inside the
// worker container before forgejo-runner consumes it.
const tokenPath = "/tmp/tok"

// ExecDispatcher implements orchestrator.Dispatcher by driving a worker
// container over `docker exec` instead of SSH. It deliberately does not
// implement orchestrator.HostKeyPinner: docker exec has no SSH host key, so the
// trust-on-first-use machinery does not apply.
type ExecDispatcher struct {
	api         dockerAPI
	forgejoURL  string
	labels      []string
	waitTimeout time.Duration
}

// NewExecDispatcher constructs a dispatcher over the given Docker API. The
// forgejoURL and labels are passed to forgejo-runner one-job; waitTimeout
// bounds WaitReady's poll for the container to enter the running state.
func NewExecDispatcher(api dockerAPI, forgejoURL string, labels []string, waitTimeout time.Duration) *ExecDispatcher {
	return &ExecDispatcher{
		api:         api,
		forgejoURL:  forgejoURL,
		labels:      labels,
		waitTimeout: waitTimeout,
	}
}

// NewExecDispatcherFromEnv builds an ExecDispatcher backed by a real Docker
// client constructed from the standard Docker environment, for use by main.
func NewExecDispatcherFromEnv(forgejoURL string, labels []string, waitTimeout time.Duration) (*ExecDispatcher, error) {
	api, err := newRealClient()
	if err != nil {
		return nil, err
	}
	return NewExecDispatcher(api, forgejoURL, labels, waitTimeout), nil
}

// WaitReady polls ContainerInspect until the container is running, bounded by
// waitTimeout and the context.
func (e *ExecDispatcher) WaitReady(ctx context.Context, id, _ string) error {
	deadline := time.Now().Add(e.waitTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := e.api.ContainerInspect(ctx, id)
		switch {
		case err != nil:
			lastErr = err
		case resp.State != nil && resp.State.Running:
			return nil
		default:
			lastErr = fmt.Errorf("container %s not running", id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("timed out")
	}
	return fmt.Errorf("docker: container %s not ready within %s: %w", id, e.waitTimeout, lastErr)
}

// RunJob writes the one-shot token to the container via stdin, then runs
// forgejo-runner one-job over `docker exec`, streaming to completion and
// failing on a nonzero exit code.
func (e *ExecDispatcher) RunJob(ctx context.Context, id, _ string, reg forgejo.Registration, job forgejo.WaitingJob) error {
	// Write the token via stdin so it never appears on an argv / process list.
	if err := e.exec(ctx, id,
		[]string{"sh", "-c", "cat > " + tokenPath + " && chmod 600 " + tokenPath},
		strings.NewReader(reg.Token),
	); err != nil {
		return fmt.Errorf("write token: %w", err)
	}

	cmd := []string{
		"forgejo-runner", "one-job",
		"--url", e.forgejoURL,
		"--uuid", reg.UUID,
		"--token-url", "file:" + tokenPath,
		"--label", strings.Join(e.labels, ","),
		"--handle", job.Handle,
		"--wait",
	}
	if err := e.exec(ctx, id, cmd, nil); err != nil {
		return fmt.Errorf("one-job: %w", err)
	}
	return nil
}

// exec runs cmd inside container id via the Docker exec API, optionally feeding
// stdin, streaming output to completion, and returning an error on a nonzero
// exit code or cancelled context.
func (e *ExecDispatcher) exec(ctx context.Context, id string, cmd []string, stdin io.Reader) error {
	execID, err := e.api.ContainerExecCreate(ctx, id, container.ExecOptions{
		AttachStdin:  stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	})
	if err != nil {
		return fmt.Errorf("exec create: %w", err)
	}
	hijack, err := e.api.ContainerExecAttach(ctx, execID)
	if err != nil {
		return fmt.Errorf("exec attach: %w", err)
	}
	defer hijack.Close()

	if err := e.pump(ctx, hijack, stdin); err != nil {
		return err
	}

	inspect, err := e.api.ContainerExecInspect(ctx, execID)
	if err != nil {
		return fmt.Errorf("exec inspect: %w", err)
	}
	if inspect.Running {
		return errors.New("exec still running after stream closed")
	}
	if inspect.ExitCode != 0 {
		return fmt.Errorf("exec exited with code %d", inspect.ExitCode)
	}
	return nil
}

// pump feeds stdin (if any) to the exec session and drains its multiplexed
// output to completion, honouring context cancellation by closing the hijacked
// connection so the read unblocks.
func (e *ExecDispatcher) pump(ctx context.Context, hijack types.HijackedResponse, stdin io.Reader) error {
	if stdin != nil {
		if _, err := io.Copy(hijack.Conn, stdin); err != nil {
			return fmt.Errorf("write stdin: %w", err)
		}
	}
	// Signal EOF on stdin so a command like `cat` terminates.
	_ = hijack.CloseWrite()

	// Closing the hijacked connection unblocks the read below, so a cancelled
	// context interrupts even a long-running `one-job --wait` instead of
	// leaking the dispatch goroutine. The watcher exits via done on return.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			hijack.Close()
		case <-done:
		}
	}()

	if _, err := stdcopy.StdCopy(io.Discard, io.Discard, hijack.Reader); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read output: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
