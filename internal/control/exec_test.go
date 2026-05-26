package control_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/internal/control"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
	"github.com/hstern/fj-bellows/internal/orchestrator"
)

func TestExecOnWorker_RPC_DisabledByDefault(t *testing.T) {
	be := &mockctl.Backend{}
	_, client := newTestServer(t, be)
	_, err := client.ExecOnWorker(t.Context(), connect.NewRequest(&controlv1.ExecOnWorkerRequest{
		InstanceId: "100",
		Command:    "true",
	}))
	if err == nil {
		t.Fatal("ExecOnWorker without writes-enabled should error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied, got %v", err)
	}
	if be.ExecOnWorkerCalls() != 0 {
		t.Fatalf("backend should not have been called; calls=%d", be.ExecOnWorkerCalls())
	}
}

func TestExecOnWorker_RPC_HappyPath(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetExecOnWorker(func(_ context.Context, id, cmd string) ([]byte, []byte, int32, int64, int64, error) {
		if id != "100" {
			t.Errorf("id: want 100 got %q", id)
		}
		if cmd != "uname -a" {
			t.Errorf("cmd: want %q got %q", "uname -a", cmd)
		}
		return []byte("Linux\n"), []byte(""), 0, 0, 0, nil
	})
	_, client := newWritesServer(t, be, true)
	resp, err := client.ExecOnWorker(t.Context(), connect.NewRequest(&controlv1.ExecOnWorkerRequest{
		InstanceId: "100",
		Command:    "uname -a",
	}))
	if err != nil {
		t.Fatalf("ExecOnWorker: %v", err)
	}
	if string(resp.Msg.Stdout) != "Linux\n" {
		t.Fatalf("stdout: want %q got %q", "Linux\n", resp.Msg.Stdout)
	}
	if resp.Msg.ExitCode != 0 {
		t.Fatalf("exit_code: want 0 got %d", resp.Msg.ExitCode)
	}
}

func TestExecOnWorker_RPC_NonZeroExitIsResponseNotError(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetExecOnWorker(func(context.Context, string, string) ([]byte, []byte, int32, int64, int64, error) {
		return []byte(""), []byte("no such file\n"), 1, 0, 0, nil
	})
	_, client := newWritesServer(t, be, true)
	resp, err := client.ExecOnWorker(t.Context(), connect.NewRequest(&controlv1.ExecOnWorkerRequest{
		InstanceId: "100",
		Command:    "cat /nope",
	}))
	if err != nil {
		t.Fatalf("ExecOnWorker: %v", err)
	}
	if resp.Msg.ExitCode != 1 {
		t.Fatalf("exit_code: want 1 got %d", resp.Msg.ExitCode)
	}
	if !strings.Contains(string(resp.Msg.Stderr), "no such file") {
		t.Fatalf("stderr: missing expected text; got %q", resp.Msg.Stderr)
	}
}

func TestExecOnWorker_RPC_TruncationPropagates(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetExecOnWorker(func(context.Context, string, string) ([]byte, []byte, int32, int64, int64, error) {
		return []byte("abc"), []byte("xy"), 0, 1024 * 1024 * 2, 5, nil
	})
	_, client := newWritesServer(t, be, true)
	resp, err := client.ExecOnWorker(t.Context(), connect.NewRequest(&controlv1.ExecOnWorkerRequest{
		InstanceId: "100",
		Command:    "yes",
	}))
	if err != nil {
		t.Fatalf("ExecOnWorker: %v", err)
	}
	if resp.Msg.TruncatedStdout != 1024*1024*2 {
		t.Fatalf("truncated_stdout: want %d got %d", 1024*1024*2, resp.Msg.TruncatedStdout)
	}
	if resp.Msg.TruncatedStderr != 5 {
		t.Fatalf("truncated_stderr: want 5 got %d", resp.Msg.TruncatedStderr)
	}
}

func TestExecOnWorker_RPC_MissingInstanceIDIsInvalidArgument(t *testing.T) {
	be := &mockctl.Backend{}
	_, client := newWritesServer(t, be, true)
	_, err := client.ExecOnWorker(t.Context(), connect.NewRequest(&controlv1.ExecOnWorkerRequest{Command: "true"}))
	if err == nil {
		t.Fatal("missing instance_id should error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument, got %v", err)
	}
}

func TestExecOnWorker_RPC_MissingCommandIsInvalidArgument(t *testing.T) {
	be := &mockctl.Backend{}
	_, client := newWritesServer(t, be, true)
	_, err := client.ExecOnWorker(t.Context(), connect.NewRequest(&controlv1.ExecOnWorkerRequest{InstanceId: "100"}))
	if err == nil {
		t.Fatal("missing command should error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument, got %v", err)
	}
}

func TestExecOnWorker_RPC_OversizedCommandIsInvalidArgument(t *testing.T) {
	be := &mockctl.Backend{}
	_, client := newWritesServer(t, be, true)
	big := strings.Repeat("a", 64*1024+1)
	_, err := client.ExecOnWorker(t.Context(), connect.NewRequest(&controlv1.ExecOnWorkerRequest{
		InstanceId: "100",
		Command:    big,
	}))
	if err == nil {
		t.Fatal("oversized command should error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument, got %v", err)
	}
}

// TestExecOnWorker_RPC_ErrorMapping pins how backend error strings (and
// the ErrExecNotSupported sentinel) map to Connect codes.
func TestExecOnWorker_RPC_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"not in pool", errors.New(`instance "ghost" not in pool`), connect.CodeNotFound},
		{"vanished", errors.New(`instance "100" vanished from pool`), connect.CodeNotFound},
		{"wrong state", errors.New(`instance "100" in state "provisioning"; exec requires idle or busy`), connect.CodeFailedPrecondition},
		{"unsupported", orchestrator.ErrExecNotSupported, connect.CodeUnimplemented},
		{"ssh dial", errors.New("ssh dial 10.0.0.5: i/o timeout"), connect.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := &mockctl.Backend{}
			be.SetExecOnWorker(func(context.Context, string, string) ([]byte, []byte, int32, int64, int64, error) {
				return nil, nil, 0, 0, 0, tc.err
			})
			_, client := newWritesServer(t, be, true)
			_, err := client.ExecOnWorker(t.Context(), connect.NewRequest(&controlv1.ExecOnWorkerRequest{
				InstanceId: "100",
				Command:    "true",
			}))
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *connect.Error
			if !errors.As(err, &ce) || ce.Code() != tc.want {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// guard so the import stays load-bearing if a future change drops it.
var _ = control.Backend(nil)
