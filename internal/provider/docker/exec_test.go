package docker

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

	"github.com/hstern/fj-bellows/internal/forgejo"
	"github.com/hstern/fj-bellows/internal/orchestrator"
)

// Compile-time guard: ExecDispatcher is an orchestrator.Dispatcher.
var _ orchestrator.Dispatcher = (*ExecDispatcher)(nil)

// TestNotHostKeyPinner asserts the docker dispatcher does NOT implement the
// SSH-only HostKeyPinner capability.
func TestNotHostKeyPinner(t *testing.T) {
	var d any = (*ExecDispatcher)(nil)
	if _, ok := d.(orchestrator.HostKeyPinner); ok {
		t.Error("ExecDispatcher must not implement orchestrator.HostKeyPinner")
	}
}

func TestWaitReadyRunning(t *testing.T) {
	api := &fakeAPI{inspectRunning: true}
	d := NewExecDispatcher(api, "https://forgejo.example.com", []string{"docker"}, time.Second)
	if err := d.WaitReady(context.Background(), "container123", ""); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestWaitReadyTimeout(t *testing.T) {
	api := &fakeAPI{inspectRunning: false}
	d := NewExecDispatcher(api, "https://forgejo.example.com", nil, 100*time.Millisecond)
	if err := d.WaitReady(context.Background(), "container123", ""); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWaitReadyCtxCancel(t *testing.T) {
	api := &fakeAPI{inspectRunning: false}
	d := NewExecDispatcher(api, "https://forgejo.example.com", nil, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.WaitReady(ctx, "container123", ""); err == nil {
		t.Fatal("expected context error")
	}
}

func TestRunJobWritesTokenAndRunsOneJob(t *testing.T) {
	api := &fakeAPI{}
	d := NewExecDispatcher(api, "https://forgejo.example.com", []string{"docker", "ubuntu-latest"}, time.Second)
	reg := forgejo.Registration{UUID: "u-1", Token: "tok-secret"}
	job := forgejo.WaitingJob{Handle: "h-1"}
	if err := d.RunJob(context.Background(), "container123", "", reg, job); err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	if len(api.execCalls) != 2 {
		t.Fatalf("exec calls = %d, want 2", len(api.execCalls))
	}

	// First exec: token write via stdin (token never appears in argv).
	tokenCmd := api.execCalls[0].opts.Cmd
	if len(tokenCmd) != 3 || tokenCmd[0] != "sh" || tokenCmd[1] != "-c" {
		t.Errorf("token cmd = %v", tokenCmd)
	}
	if slices.Contains(tokenCmd, "tok-secret") {
		t.Error("token must not appear on argv")
	}
	if !api.execCalls[0].opts.AttachStdin {
		t.Error("token exec must attach stdin")
	}
	if got := api.firstStdin(); got != "tok-secret" {
		t.Errorf("stdin = %q, want token", got)
	}

	// Second exec: forgejo-runner one-job with the expected args.
	jobCmd := api.execCalls[1].opts.Cmd
	want := []string{
		"forgejo-runner", "one-job",
		"--url", "https://forgejo.example.com",
		"--uuid", "u-1",
		"--token-url", "file:/tmp/tok",
		"--label", "docker,ubuntu-latest",
		"--handle", "h-1",
		"--wait",
	}
	if !slices.Equal(jobCmd, want) {
		t.Errorf("one-job cmd =\n %v\nwant\n %v", jobCmd, want)
	}
}

func TestRunJobNonzeroExit(t *testing.T) {
	// Token write succeeds (exit 0), one-job exits nonzero.
	api := &execExitAPI{fakeAPI: &fakeAPI{}, exits: []int{0, 7}}
	d := NewExecDispatcher(api, "https://forgejo.example.com", nil, time.Second)
	err := d.RunJob(context.Background(), "c", "", forgejo.Registration{UUID: "u", Token: "t"}, forgejo.WaitingJob{Handle: "h"})
	if err == nil {
		t.Fatal("expected error for nonzero exit code")
	}
}

func TestRunJobCtxCancel(t *testing.T) {
	api := &blockingAPI{fakeAPI: &fakeAPI{}, attached: make(chan struct{})}
	d := NewExecDispatcher(api, "https://forgejo.example.com", nil, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-api.attached
		cancel()
	}()
	err := d.RunJob(ctx, "c", "", forgejo.Registration{UUID: "u", Token: "t"}, forgejo.WaitingJob{Handle: "h"})
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

// execExitAPI returns a sequence of exit codes from ContainerExecInspect.
type execExitAPI struct {
	*fakeAPI
	exits []int
	n     int
}

func (a *execExitAPI) ContainerExecInspect(_ context.Context, _ string) (container.ExecInspect, error) {
	code := 0
	if a.n < len(a.exits) {
		code = a.exits[a.n]
	}
	a.n++
	return container.ExecInspect{ExitCode: code}, nil
}

// blockingAPI attaches an exec whose output read blocks until the connection is
// closed, modelling a long-running `one-job --wait` that ctx cancel must abort.
type blockingAPI struct {
	*fakeAPI
	attached chan struct{}
	once     bool
}

func (a *blockingAPI) ContainerExecAttach(_ context.Context, _ string) (types.HijackedResponse, error) {
	const mediaType = "application/vnd.docker.multiplexed-stream"
	if !a.once {
		// First exec is the token write: complete immediately (EOF read).
		a.once = true
		return types.NewHijackedResponse(&fakeConn{out: &a.stdin, capture: a.fakeAPI}, mediaType), nil
	}
	// Second exec is one-job: block until ctx cancel closes the connection.
	close(a.attached)
	return types.NewHijackedResponse(newBlockingConn(), mediaType), nil
}

// blockingConn blocks reads until Close is called, then returns EOF.
type blockingConn struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func newBlockingConn() *blockingConn { return &blockingConn{closed: make(chan struct{})} }

func (c *blockingConn) Read(_ []byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}
func (c *blockingConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *blockingConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (c *blockingConn) CloseWrite() error                  { return nil }
func (c *blockingConn) LocalAddr() net.Addr                { return fakeAddr{} }
func (c *blockingConn) RemoteAddr() net.Addr               { return fakeAddr{} }
func (c *blockingConn) SetDeadline(_ time.Time) error      { return nil }
func (c *blockingConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *blockingConn) SetWriteDeadline(_ time.Time) error { return nil }
