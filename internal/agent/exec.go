package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"syscall"

	"connectrpc.com/connect"

	agentv1 "github.com/hstern/fj-bellows/gen/fjbellows/agent/v1"
)

// Exec implements AgentService.Exec for the agent. Phase A (FJB-93)
// supports **pipe mode only** — ExecOpen.tty=true is rejected with
// CodeUnimplemented. PTY allocation and window-resize handling land in
// FJB-93 Phase C.
//
// Concurrency: the handler launches three goroutines that share the
// stream's Send side (stdout pump, stderr pump, terminal Exit emit) and
// one that owns Receive. Send-side serialization goes through a buffered
// channel + single sender goroutine; that avoids manual mutexing of
// connect.BidiStream.Send and gives us bounded backpressure for free.
func (h *Handler) Exec(ctx context.Context, stream *connect.BidiStream[agentv1.ShellMsg, agentv1.ShellEvent]) error {
	open, err := receiveOpen(stream)
	if err != nil {
		return err
	}

	if open.GetTty() {
		return connect.NewError(connect.CodeUnimplemented, errors.New("PTY mode is not yet implemented (FJB-93 Phase C)"))
	}
	if len(open.GetArgv()) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("argv must be non-empty"))
	}

	// Single send goroutine owns the stream's send side. Buffer is sized
	// so a brief stall on the network doesn't immediately backpressure
	// the stdout/stderr pumps inside the agent.
	sendBuf := make(chan *agentv1.ShellEvent, 64)
	sendDone := make(chan struct{})
	sendErr := make(chan error, 1)
	go runSender(stream, sendBuf, sendDone, sendErr)

	// Sentinel frame matches the StreamEvents convention — open call
	// returns immediately even before fork+exec completes.
	sendBuf <- &agentv1.ShellEvent{
		Kind: &agentv1.ShellEvent_Opened{Opened: &agentv1.ExecOpened{}},
	}

	// Build environment: agent process env is NOT inherited; only the
	// explicit map in Open is set. Matches the design doc's "no implicit
	// env passthrough" rule.
	env := make([]string, 0, len(open.GetEnv()))
	for k, v := range open.GetEnv() {
		env = append(env, k+"="+v)
	}

	//nolint:gosec // G204: argv is supplied by an authenticated orchestrator caller; that's the whole point of this RPC.
	cmd := exec.CommandContext(ctx, open.GetArgv()[0], open.GetArgv()[1:]...)
	cmd.Env = env
	// New process group so signals can be delivered to the whole tree
	// (sh -c "..." spawning children, for example).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Wire stdout/stderr via custom Writers rather than StdoutPipe /
	// StderrPipe. exec.Cmd documents that Wait closes pipes it created,
	// which races our pump goroutines on fast-exiting commands; the
	// Writer path uses exec's internal copy goroutines instead, which
	// Wait blocks on before returning. That makes the "all output
	// flushed before Exit" ordering structural rather than timing-based.
	cmd.Stdout = &streamWriter{ch: sendBuf, wrap: asStdout}
	cmd.Stderr = &streamWriter{ch: sendBuf, wrap: asStderr}

	stdinW, err := cmd.StdinPipe()
	if err != nil {
		close(sendBuf)
		<-sendDone
		return connect.NewError(connect.CodeInternal, fmt.Errorf("stdin pipe: %w", err))
	}

	if err := cmd.Start(); err != nil {
		// Mirror sh's convention: command-not-found is exit 127.
		_ = stdinW.Close()
		sendBuf <- exitEvent(127, "")
		close(sendBuf)
		<-sendDone
		// The RPC itself succeeded; the child's failure is on-stream.
		//nolint:nilerr // start failure is reported as on-stream Exit{127}, not as an RPC error
		return nil
	}

	// Client-side reader: stdin frames, signals, CloseStdin. Runs until
	// the client closes its send side OR the child exits. Returns the
	// receive error so the outer scope can decide whether to log it.
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		runReceiver(stream, cmd, stdinW)
	}()

	// Wait for child to exit. cmd.Wait blocks until both the child has
	// exited AND exec's internal stdout/stderr copy goroutines have
	// drained — so all output is in the sendBuf channel before this
	// returns. The CommandContext-bound cancellation covers the "RPC
	// ctx canceled" case: the kernel sends SIGKILL to the process group
	// and Wait returns shortly after.
	waitErr := cmd.Wait()

	sendBuf <- buildExitEvent(waitErr)
	close(sendBuf)

	// The receiver goroutine may still be blocked on stream.Receive().
	// On the agent side we don't have a way to forcibly close the
	// receive half from a handler; rely on the client to close its send
	// side or on the transport to tear down when we return. Either way,
	// the goroutine exits when Receive returns an error.
	_ = recvDone

	<-sendDone
	if err := drainSendErr(sendErr); err != nil {
		slog.Default().Debug("agent exec send error", "err", err)
	}
	return nil
}

// receiveOpen reads the first frame and asserts it is an ExecOpen. The
// design doc says Open must be the first message exactly once; anything
// else here is a client bug we surface as InvalidArgument.
func receiveOpen(stream *connect.BidiStream[agentv1.ShellMsg, agentv1.ShellEvent]) (*agentv1.ExecOpen, error) {
	msg, err := stream.Receive()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("stream closed before ExecOpen"))
		}
		return nil, err
	}
	open, ok := msg.GetKind().(*agentv1.ShellMsg_Open)
	if !ok || open.Open == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("first frame must be ExecOpen"))
	}
	return open.Open, nil
}

// runSender owns the stream's send side. Reads events from sendBuf and
// forwards each to stream.Send. Records the first send error and exits
// when sendBuf is closed.
func runSender(stream *connect.BidiStream[agentv1.ShellMsg, agentv1.ShellEvent], sendBuf <-chan *agentv1.ShellEvent, done chan<- struct{}, errCh chan<- error) {
	defer close(done)
	for ev := range sendBuf {
		if err := stream.Send(ev); err != nil {
			select {
			case errCh <- err:
			default:
			}
			// Drain the channel so producers don't block, but don't
			// send anything else — the stream is dead.
			for range sendBuf { //nolint:revive // intentional drain
			}
			return
		}
	}
}

// streamWriter is an io.Writer that emits each Write as a single
// ShellEvent into sendBuf. exec.Cmd's internal copy loop calls Write
// with chunks up to ~32 KiB by default, which lines up with the design
// doc's frame budget. The buffer cannot be reused across Writes since
// Connect's marshaller may reference it asynchronously, so we copy.
type streamWriter struct {
	ch   chan<- *agentv1.ShellEvent
	wrap func([]byte) *agentv1.ShellEvent
}

func (w *streamWriter) Write(p []byte) (int, error) {
	chunk := make([]byte, len(p))
	copy(chunk, p)
	w.ch <- w.wrap(chunk)
	return len(p), nil
}

func asStdout(data []byte) *agentv1.ShellEvent {
	return &agentv1.ShellEvent{Kind: &agentv1.ShellEvent_Stdout{Stdout: &agentv1.ExecStdout{Data: data}}}
}

func asStderr(data []byte) *agentv1.ShellEvent {
	return &agentv1.ShellEvent{Kind: &agentv1.ShellEvent_Stderr{Stderr: &agentv1.ExecStderr{Data: data}}}
}

// runReceiver processes client→server frames after Open: Stdin (write to
// child stdin), Signal (deliver to process group), CloseStdin (close
// child stdin EOF), Resize (logged + ignored in pipe mode).
func runReceiver(stream *connect.BidiStream[agentv1.ShellMsg, agentv1.ShellEvent], cmd *exec.Cmd, stdinW io.WriteCloser) {
	for {
		msg, err := stream.Receive()
		if err != nil {
			// Either the client closed send-side cleanly (io.EOF) or
			// the transport died. Close stdin so the child sees EOF
			// in either case; if the child already exited this is a
			// no-op on an already-closed pipe.
			_ = stdinW.Close()
			return
		}
		switch kind := msg.GetKind().(type) {
		case *agentv1.ShellMsg_Stdin:
			if kind.Stdin == nil {
				continue
			}
			if _, werr := stdinW.Write(kind.Stdin.GetData()); werr != nil {
				// Child closed stdin (probably exited). Drop further
				// stdin frames silently.
				_ = stdinW.Close()
			}
		case *agentv1.ShellMsg_CloseStdin:
			_ = stdinW.Close()
		case *agentv1.ShellMsg_Signal:
			deliverSignal(cmd, kind.Signal.GetName())
		case *agentv1.ShellMsg_Resize:
			// Pipe mode: Resize is a no-op. Log at debug so a curious
			// operator can confirm the frame arrived.
			slog.Default().Debug("agent exec: ignoring Resize in pipe mode")
		case *agentv1.ShellMsg_Open:
			// A second Open is a protocol violation; close stdin to
			// nudge the child to exit and let cmd.Wait clean up.
			slog.Default().Warn("agent exec: duplicate ExecOpen frame; closing stdin")
			_ = stdinW.Close()
			return
		}
	}
}

// deliverSignal sends a signal to the child's process group. Unknown
// names are dropped (logged). The Setpgid setup in Exec means the
// negative-pid kill targets every descendant.
func deliverSignal(cmd *exec.Cmd, name string) {
	sig := signalByName(name)
	if sig == 0 {
		slog.Default().Debug("agent exec: unknown signal name", "name", name)
		return
	}
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		// Fall back to single-process kill.
		_ = cmd.Process.Signal(sig)
		return
	}
	_ = syscall.Kill(-pgid, sig)
}

func signalByName(name string) syscall.Signal {
	switch name {
	case "SIGINT":
		return syscall.SIGINT
	case "SIGTERM":
		return syscall.SIGTERM
	case "SIGQUIT":
		return syscall.SIGQUIT
	case "SIGHUP":
		return syscall.SIGHUP
	default:
		return 0
	}
}

func exitEvent(code int32, sig string) *agentv1.ShellEvent {
	return &agentv1.ShellEvent{
		Kind: &agentv1.ShellEvent_Exit{Exit: &agentv1.ExecExit{Code: code, Signal: sig}},
	}
}

// buildExitEvent inspects cmd.Wait's error and produces the terminal
// ExecExit frame. Three cases: clean exit, signal kill, other error.
func buildExitEvent(waitErr error) *agentv1.ShellEvent {
	if waitErr == nil {
		return exitEvent(0, "")
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		ws, ok := exitErr.Sys().(syscall.WaitStatus)
		if ok {
			if ws.Signaled() {
				return exitEvent(int32(128+int(ws.Signal())), ws.Signal().String()) //nolint:gosec // signum is 1..31, no overflow
			}
			return exitEvent(int32(ws.ExitStatus()), "") //nolint:gosec // exit code is 0..255 on Linux
		}
		return exitEvent(int32(exitErr.ExitCode()), "") //nolint:gosec // ExitCode is 0..255 on POSIX
	}
	// Unknown error from Wait — surface as a non-zero exit so the
	// client doesn't conclude success.
	return exitEvent(-1, "")
}

// drainSendErr returns the first send error (if any) without blocking.
func drainSendErr(errCh <-chan error) error {
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}
