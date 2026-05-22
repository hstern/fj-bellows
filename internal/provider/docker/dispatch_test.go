package docker

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/forgejo"
	"github.com/hstern/fj-bellows/internal/orchestrator"
)

// Compile-time guarantees: ExecDispatcher satisfies orchestrator.Dispatcher,
// and deliberately does NOT implement orchestrator.HostKeyPinner (which is an
// SSH-only optional capability). If a future change accidentally added a
// PinHostKey method, this test would no longer compile.
var _ orchestrator.Dispatcher = (*ExecDispatcher)(nil)

func TestNotAHostKeyPinner(t *testing.T) {
	var d orchestrator.Dispatcher = &ExecDispatcher{}
	if _, ok := d.(orchestrator.HostKeyPinner); ok {
		t.Error("ExecDispatcher must not implement HostKeyPinner (SSH-only capability)")
	}
}

func TestWaitReadySuccess(t *testing.T) {
	// Container reports "false" twice, then "true" — exercises the poll loop.
	var calls atomic.Int32
	fc := &fakeCLI{onCall: func(args []string) ([]byte, error) {
		if args[0] != "inspect" {
			t.Errorf("unexpected docker subcommand: %v", args)
		}
		n := calls.Add(1)
		if n < 3 {
			return []byte("false\n"), nil
		}
		return []byte("true\n"), nil
	}}
	d := NewExecDispatcher(fc, "docker", "https://forgejo.example/", []string{"x"}, 2*time.Second)
	d.pollInterval = 5 * time.Millisecond

	if err := d.WaitReady(context.Background(), containerID, ""); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("expected at least 3 polls, got %d", got)
	}
}

func TestWaitReadyTimeout(t *testing.T) {
	fc := &fakeCLI{onCall: func(_ []string) ([]byte, error) {
		return []byte("false"), nil
	}}
	d := NewExecDispatcher(fc, "docker", "u", nil, 30*time.Millisecond)
	d.pollInterval = 5 * time.Millisecond
	err := d.WaitReady(context.Background(), containerID, "")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWaitReadyCtxCanceled(t *testing.T) {
	fc := &fakeCLI{onCall: func(_ []string) ([]byte, error) {
		return []byte("false"), nil
	}}
	d := NewExecDispatcher(fc, "docker", "u", nil, time.Minute)
	d.pollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := d.WaitReady(ctx, containerID, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunJobDeliversTokenViaStdin(t *testing.T) {
	fc := &fakeCLI{
		responses: []fakeResponse{
			{stdout: nil}, // first call: write token via stdin
			{stdout: nil}, // second call: forgejo-runner one-job
		},
	}
	d := NewExecDispatcher(fc, "docker", "https://forgejo.example/", []string{"label-a", "label-b"}, time.Second)
	reg := forgejo.Registration{UUID: "uuid-1", Token: "secret-token"}
	job := forgejo.WaitingJob{Handle: "handle-1"}

	if err := d.RunJob(context.Background(), containerID, "", reg, job); err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	calls := fc.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}

	// Call 1: docker exec -i <id> sh -c 'cat > /tmp/tok && chmod 600 /tmp/tok'
	// Token MUST come via stdin, not argv.
	c1 := calls[0]
	if c1.stdin != "secret-token" {
		t.Errorf("token stdin = %q", c1.stdin)
	}
	wantPrefix := []string{"exec", "-i", containerID, "sh", "-c"}
	for i, w := range wantPrefix {
		if c1.args[i] != w {
			t.Errorf("call1 args[%d] = %q, want %q", i, c1.args[i], w)
		}
	}
	if !strings.Contains(c1.args[5], "chmod 600 /tmp/tok") {
		t.Errorf("call1 cmd = %q", c1.args[5])
	}
	// Token MUST NOT appear in argv anywhere.
	for _, a := range c1.args {
		if strings.Contains(a, "secret-token") {
			t.Errorf("token leaked into argv: %v", c1.args)
		}
	}

	// Call 2: docker exec <id> forgejo-runner one-job --url ... --label label-a,label-b --handle handle-1 --wait
	c2 := calls[1]
	want2 := []string{
		"exec", containerID,
		"forgejo-runner", "one-job",
		"--url", "https://forgejo.example/",
		"--uuid", "uuid-1",
		"--token-url", "file:/tmp/tok",
		"--label", "label-a,label-b",
		"--handle", "handle-1",
		"--wait",
	}
	if strings.Join(c2.args, " ") != strings.Join(want2, " ") {
		t.Errorf("call2 args = %v\nwant %v", c2.args, want2)
	}
}

func TestRunJobTokenWriteError(t *testing.T) {
	fc := &fakeCLI{responses: []fakeResponse{{err: errors.New("write fail")}}}
	d := NewExecDispatcher(fc, "docker", "u", nil, time.Second)
	err := d.RunJob(context.Background(), containerID, "", forgejo.Registration{Token: "t"}, forgejo.WaitingJob{})
	if err == nil || !strings.Contains(err.Error(), "write token") {
		t.Fatalf("err = %v, want 'write token' error", err)
	}
}

func TestRunJobOneJobError(t *testing.T) {
	fc := &fakeCLI{
		responses: []fakeResponse{
			{stdout: nil},               // token write succeeds
			{err: errors.New("exit 1")}, // one-job fails
		},
	}
	d := NewExecDispatcher(fc, "docker", "u", nil, time.Second)
	err := d.RunJob(context.Background(), containerID, "", forgejo.Registration{Token: "t"}, forgejo.WaitingJob{})
	if err == nil || !strings.Contains(err.Error(), "one-job") {
		t.Fatalf("err = %v, want 'one-job' error", err)
	}
}

func TestRunJobCtxCancel(t *testing.T) {
	fc := &fakeCLI{
		responses: []fakeResponse{
			{stdout: nil},
			{err: context.Canceled},
		},
	}
	d := NewExecDispatcher(fc, "docker", "u", nil, time.Second)
	err := d.RunJob(context.Background(), containerID, "", forgejo.Registration{Token: "t"}, forgejo.WaitingJob{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNewDefaultRunner(t *testing.T) {
	if got := NewDefaultRunner(""); got == nil {
		t.Fatal("NewDefaultRunner(empty) = nil")
	}
	if got := NewDefaultRunner("docker"); got == nil {
		t.Fatal("NewDefaultRunner returned nil")
	}
}
