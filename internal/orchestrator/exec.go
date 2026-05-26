package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/ssh"
)

// ExecOnWorker error sentinels. ErrExecNotSupported surfaces when the
// configured dispatcher has no SSH path (e.g. the docker provider, which
// uses docker exec); ExecOnWorker on a docker-provider worker is a
// distinct (future) RPC and not handled here.
var (
	// ErrExecNotSupported is returned when the orchestrator's dispatcher
	// is not an SSHDispatcher (so there's no SSH path to exec over).
	ErrExecNotSupported = errors.New("ExecOnWorker is not supported by this dispatcher (no SSH path)")
)

// ExecResult is the per-call outcome ExecOnWorker returns. Stdout/Stderr
// are bounded by execOutputLimit; TruncatedStdout/TruncatedStderr carry
// the original byte counts when truncation happened.
type ExecResult struct {
	Stdout          []byte
	Stderr          []byte
	ExitCode        int32
	TruncatedStdout int64
	TruncatedStderr int64
}

const (
	// execCommandLimit caps the size of the command line accepted by
	// ExecOnWorker. 64 KiB is well above any operator-typed command and
	// far below the SSH session line limit; longer requests are almost
	// certainly a misuse (a script being shoved through the wrong RPC).
	execCommandLimit = 64 * 1024

	// execOutputLimit caps the per-stream (stdout / stderr) bytes
	// returned to the operator. 1 MiB is enough for typical command
	// output while bounding a runaway `cat /dev/urandom` worth of bytes
	// being held in memory on the daemon and serialised over the wire.
	execOutputLimit = 1 * 1024 * 1024

	// execDefaultTimeout bounds the SSH session when the caller's
	// context has no deadline. Operators driving the RPC interactively
	// will usually let the default kick in; long-running ops should
	// pass a longer context deadline explicitly.
	execDefaultTimeout = 60 * time.Second
)

// ExecOnWorker runs command on the worker with instanceID via the
// orchestrator's existing SSH dispatcher. Returns stdout, stderr, exit
// code, and any orchestrator-level error (instance not in pool, wrong
// state, dispatcher doesn't support SSH, SSH dial failure, ...). Every
// call is audit-logged with the caller identity threaded through ctx
// via WithAuditCaller. The session is bound by ctx's deadline; if ctx
// has none, the orchestrator imposes execDefaultTimeout.
//
// Refuses to run when the node is in StateProvisioning (SSH may not be
// up yet) or StateRemoving (Destroy is in flight; an SSH session would
// race the teardown). StateIdle and StateBusy are both fine — a busy
// worker is running a job, but exec is an out-of-band debug poke and
// does not interfere with the dispatch session.
//
// Bounded by execCommandLimit on input and execOutputLimit per output
// stream. Truncation is reported via the returned ExecResult counters
// so the operator knows there was more.
func (o *Orchestrator) ExecOnWorker(ctx context.Context, instanceID, command string) (ExecResult, error) {
	o.log.Info("exec-on-worker requested", "id", instanceID, "caller", auditCallerFromCtx(ctx))

	if len(command) > execCommandLimit {
		return ExecResult{}, fmt.Errorf("command too long: %d bytes (limit %d)", len(command), execCommandLimit)
	}

	disp, ok := o.disp.(*SSHDispatcher)
	if !ok {
		return ExecResult{}, ErrExecNotSupported
	}

	n, ok := o.pool.Get(instanceID)
	if !ok {
		return ExecResult{}, fmt.Errorf("instance %q not in pool", instanceID)
	}
	switch n.State {
	case StateIdle, StateBusy:
		// OK to exec.
	case StateProvisioning, StateDraining, StateRemoving:
		return ExecResult{}, fmt.Errorf("instance %q in state %q; exec requires idle or busy", instanceID, n.State)
	default:
		return ExecResult{}, fmt.Errorf("instance %q in state %q; exec requires idle or busy", instanceID, n.State)
	}

	// Impose a default deadline so a hung remote command can't pin the
	// dispatch goroutine forever when the caller didn't set one.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, execDefaultTimeout)
		defer cancel()
	}

	client, err := disp.dial(ctx, n.IP)
	if err != nil {
		return ExecResult{}, fmt.Errorf("ssh dial %s: %w", n.IP, err)
	}
	defer func() { _ = client.Close() }()

	res, err := runRemoteCapture(ctx, client, command)
	if err != nil {
		return ExecResult{}, err
	}
	return res, nil
}

// runRemoteCapture runs cmd inside `sh -c <cmd>` on client, returning
// stdout/stderr (each truncated to execOutputLimit) and the remote exit
// code. A non-zero exit is NOT an error here — it's the normal way a
// shell command signals failure and the operator wants to see it.
// SSH-level failures (session open, transport drop, ctx cancellation)
// are surfaced as errors.
func runRemoteCapture(ctx context.Context, client *ssh.Client, cmd string) (ExecResult, error) {
	sess, err := client.NewSession()
	if err != nil {
		return ExecResult{}, fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	// Cap output by writing through limited buffers; truncated_* fields
	// carry the original byte counts so the operator can tell when output
	// was clipped.
	stdout := &cappedBuffer{limit: execOutputLimit}
	stderr := &cappedBuffer{limit: execOutputLimit}
	sess.Stdout = stdout
	sess.Stderr = stderr

	// Closing the session unblocks Run on ctx cancellation; the watcher
	// exits via done when Run returns.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-done:
		}
	}()

	// `sh -c` lets the operator pass pipelines, redirects, etc., as one
	// command string; shellQuote keeps attacker-influenced bytes from
	// breaking out of the quoting.
	wrapped := "sh -c " + shellQuote(cmd)
	runErr := sess.Run(wrapped)

	res := ExecResult{
		Stdout:          stdout.Bytes(),
		Stderr:          stderr.Bytes(),
		TruncatedStdout: stdout.original(),
		TruncatedStderr: stderr.original(),
	}

	if runErr == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return ExecResult{}, ctx.Err()
	}
	// A remote non-zero exit is signalled via *ssh.ExitError; surface
	// the code in the response rather than as an orchestrator error.
	var exitErr *ssh.ExitError
	if errors.As(runErr, &exitErr) {
		//nolint:gosec // remote exit codes are bounded to a uint8 by POSIX; int32 fits trivially
		res.ExitCode = int32(exitErr.ExitStatus())
		return res, nil
	}
	return ExecResult{}, fmt.Errorf("run remote command: %w", runErr)
}

// cappedBuffer is an io.Writer that records at most limit bytes while
// tracking the total seen. Used to bound stdout/stderr we hold in
// memory for a single ExecOnWorker response.
type cappedBuffer struct {
	limit int
	buf   bytes.Buffer
	total int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.total += int64(len(p))
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		// Pretend the write succeeded so the remote session keeps going;
		// we just stop recording past the limit.
		return len(p), nil
	}
	if len(p) <= remaining {
		c.buf.Write(p)
		return len(p), nil
	}
	c.buf.Write(p[:remaining])
	return len(p), nil
}

// Bytes returns the bounded captured bytes (a copy is not necessary —
// the buffer is only read once by the caller).
func (c *cappedBuffer) Bytes() []byte {
	return c.buf.Bytes()
}

// original returns the pre-truncation byte count IF truncation happened,
// otherwise 0. Matches the proto's truncated_* contract: "report the
// original byte count when truncation happened".
func (c *cappedBuffer) original() int64 {
	if c.total > int64(c.buf.Len()) {
		return c.total
	}
	return 0
}

// Compile-time guard: io.Writer is satisfied. Avoids a silent fall-back
// to a wrapper that copies into a fresh buffer somewhere.
var _ io.Writer = (*cappedBuffer)(nil)
