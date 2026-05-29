package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/hstern/fj-bellows/gen/fjbellows/agent/v1"
	"github.com/hstern/fj-bellows/gen/fjbellows/agent/v1/agentv1connect"
)

// execTestArgvBin is the binary used across exec tests. Extracted so
// goconst doesn't complain about repeated string literals.
const execTestArgvBin = "echo"

// execTestSetup spins up an in-process agent server reachable via H2C
// (Connect bidi streaming needs HTTP/2). Returns a connected client and
// a teardown.
func execTestSetup(t *testing.T) (agentv1connect.AgentServiceClient, func()) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("exec tests need a POSIX-y OS, not %s", runtime.GOOS)
	}
	h := NewHandler("test", time.Now())
	srv := NewServer("127.0.0.1:0", h, nil)

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.EnableHTTP2 = true
	ts.StartTLS() // bidi streaming requires HTTP/2; httptest's H2 is over TLS

	client := agentv1connect.NewAgentServiceClient(ts.Client(), ts.URL)
	return client, ts.Close
}

// runExec opens an Exec stream, sends Open, then optionally drives the
// stream via `drive`, then drains events until Exit. Returns the
// captured stdout, stderr, exit code, signal name, and any RPC error.
func runExec(t *testing.T, client agentv1connect.AgentServiceClient, open *agentv1.ExecOpen, drive func(*connect.BidiStreamForClient[agentv1.ShellMsg, agentv1.ShellEvent])) (stdout, stderr string, code int32, sig string, rpcErr error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream := client.Exec(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&agentv1.ShellMsg{Kind: &agentv1.ShellMsg_Open{Open: open}}); err != nil {
		return "", "", 0, "", err
	}

	if drive != nil {
		drive(stream)
	}

	var outBuf, errBuf strings.Builder
	for {
		ev, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return outBuf.String(), errBuf.String(), code, sig, err
		}
		switch k := ev.GetKind().(type) {
		case *agentv1.ShellEvent_Opened:
			// sentinel; ignore.
		case *agentv1.ShellEvent_Stdout:
			outBuf.Write(k.Stdout.GetData())
		case *agentv1.ShellEvent_Stderr:
			errBuf.Write(k.Stderr.GetData())
		case *agentv1.ShellEvent_Exit:
			code = k.Exit.GetCode()
			sig = k.Exit.GetSignal()
			return outBuf.String(), errBuf.String(), code, sig, nil
		}
	}
	return outBuf.String(), errBuf.String(), code, sig, nil
}

func TestExec_Echo(t *testing.T) {
	t.Parallel()
	client, teardown := execTestSetup(t)
	defer teardown()

	out, errOut, code, sig, err := runExec(t, client,
		&agentv1.ExecOpen{Argv: []string{execTestArgvBin, "hello"}}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "hello\n" {
		t.Errorf("stdout = %q, want %q", out, "hello\n")
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty", errOut)
	}
	if code != 0 || sig != "" {
		t.Errorf("exit = %d / %q, want 0 / empty", code, sig)
	}
}

func TestExec_StdinThenClose(t *testing.T) {
	t.Parallel()
	client, teardown := execTestSetup(t)
	defer teardown()

	drive := func(s *connect.BidiStreamForClient[agentv1.ShellMsg, agentv1.ShellEvent]) {
		_ = s.Send(&agentv1.ShellMsg{Kind: &agentv1.ShellMsg_Stdin{
			Stdin: &agentv1.ExecStdin{Data: []byte("hi cat\n")},
		}})
		_ = s.Send(&agentv1.ShellMsg{Kind: &agentv1.ShellMsg_CloseStdin{
			CloseStdin: &agentv1.ExecCloseStdin{},
		}})
	}
	out, _, code, sig, err := runExec(t, client,
		&agentv1.ExecOpen{Argv: []string{"cat"}}, drive)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "hi cat\n" {
		t.Errorf("stdout = %q, want %q", out, "hi cat\n")
	}
	if code != 0 || sig != "" {
		t.Errorf("exit = %d / %q, want 0 / empty", code, sig)
	}
}

func TestExec_NonZeroExit(t *testing.T) {
	t.Parallel()
	client, teardown := execTestSetup(t)
	defer teardown()

	_, _, code, sig, err := runExec(t, client,
		&agentv1.ExecOpen{Argv: []string{"sh", "-c", "exit 7"}}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 7 || sig != "" {
		t.Errorf("exit = %d / %q, want 7 / empty", code, sig)
	}
}

func TestExec_Stderr(t *testing.T) {
	t.Parallel()
	client, teardown := execTestSetup(t)
	defer teardown()

	out, errOut, code, _, err := runExec(t, client,
		&agentv1.ExecOpen{Argv: []string{"sh", "-c", "echo to-err >&2"}}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if errOut != "to-err\n" {
		t.Errorf("stderr = %q, want %q", errOut, "to-err\n")
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}

func TestExec_EnvPassthrough(t *testing.T) {
	t.Parallel()
	client, teardown := execTestSetup(t)
	defer teardown()

	out, _, code, _, err := runExec(t, client,
		&agentv1.ExecOpen{
			Argv: []string{"sh", "-c", "echo FOO=$FOO; echo PATH_LEN=${#PATH}"},
			Env:  map[string]string{"FOO": "bar", "PATH": "/usr/bin:/bin"},
		}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(out, "FOO=bar") {
		t.Errorf("stdout = %q, want it to contain FOO=bar", out)
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}

func TestExec_RejectsTTY(t *testing.T) {
	t.Parallel()
	client, teardown := execTestSetup(t)
	defer teardown()

	_, _, _, _, err := runExec(t, client,
		&agentv1.ExecOpen{Argv: []string{execTestArgvBin}, Tty: true}, nil)
	if err == nil {
		t.Fatal("Exec with tty=true succeeded, want CodeUnimplemented")
	}
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("code = %v, want CodeUnimplemented; err = %v", connect.CodeOf(err), err)
	}
}

func TestExec_RejectsEmptyArgv(t *testing.T) {
	t.Parallel()
	client, teardown := execTestSetup(t)
	defer teardown()

	_, _, _, _, err := runExec(t, client,
		&agentv1.ExecOpen{Argv: nil}, nil)
	if err == nil {
		t.Fatal("Exec with empty argv succeeded, want CodeInvalidArgument")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want CodeInvalidArgument; err = %v", connect.CodeOf(err), err)
	}
}

func TestExec_CommandNotFound(t *testing.T) {
	t.Parallel()
	client, teardown := execTestSetup(t)
	defer teardown()

	_, _, code, sig, err := runExec(t, client,
		&agentv1.ExecOpen{Argv: []string{"/no/such/binary/fjb-test-not-real"}}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 127 || sig != "" {
		t.Errorf("exit = %d / %q, want 127 / empty", code, sig)
	}
}

func TestExec_SignalKill(t *testing.T) {
	t.Parallel()
	client, teardown := execTestSetup(t)
	defer teardown()

	drive := func(s *connect.BidiStreamForClient[agentv1.ShellMsg, agentv1.ShellEvent]) {
		// Give the child a moment to actually be exec'd before we
		// signal it; otherwise the signal might race fork+exec and
		// land on a process that hasn't yet replaced itself with
		// sleep.
		time.Sleep(200 * time.Millisecond)
		_ = s.Send(&agentv1.ShellMsg{Kind: &agentv1.ShellMsg_Signal{
			Signal: &agentv1.ExecSignal{Name: "SIGTERM"},
		}})
	}
	_, _, code, sig, err := runExec(t, client,
		&agentv1.ExecOpen{Argv: []string{"sleep", "30"}}, drive)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if sig == "" {
		t.Errorf("signal = %q, want non-empty (process should have been killed); code = %d", sig, code)
	}
	// Exit code convention for signal kill: 128 + signum. SIGTERM is 15.
	if code != 128+15 {
		t.Logf("exit code = %d (informational; convention is 128+signum)", code)
	}
}

func TestExec_FirstFrameMustBeOpen(t *testing.T) {
	t.Parallel()
	client, teardown := execTestSetup(t)
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := client.Exec(ctx)
	defer func() { _ = stream.CloseRequest() }()

	// Send Stdin before Open — protocol violation.
	if err := stream.Send(&agentv1.ShellMsg{Kind: &agentv1.ShellMsg_Stdin{
		Stdin: &agentv1.ExecStdin{Data: []byte("nope")},
	}}); err != nil {
		t.Fatalf("Send Stdin: %v", err)
	}
	_, err := stream.Receive()
	if err == nil {
		t.Fatal("Receive succeeded after pre-Open Stdin, want CodeInvalidArgument")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want CodeInvalidArgument; err = %v", connect.CodeOf(err), err)
	}
}

// TestExec_BearerAuth confirms the Exec RPC is gated by the same bearer
// middleware as Health (the wrap is shared at the http.Handler layer).
func TestExec_BearerAuth(t *testing.T) {
	t.Parallel()

	h := NewHandler("test", time.Now())
	srv := NewServer("127.0.0.1:0", h, nil, WithBearerToken("s3cret"))

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()

	client := agentv1connect.NewAgentServiceClient(ts.Client(), ts.URL)
	stream := client.Exec(context.Background())
	defer func() { _ = stream.CloseRequest() }()
	_ = stream.Send(&agentv1.ShellMsg{Kind: &agentv1.ShellMsg_Open{
		Open: &agentv1.ExecOpen{Argv: []string{execTestArgvBin}},
	}})
	_, err := stream.Receive()
	if err == nil {
		t.Fatal("Exec succeeded without token, want CodeUnauthenticated")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}
}

// Confirm we're not importing http blindly.
var _ = http.NoBody
