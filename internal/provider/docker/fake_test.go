package docker

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

// execCall records one ContainerExecCreate invocation.
type execCall struct {
	id   string
	opts container.ExecOptions
}

// fakeAPI is an in-memory dockerAPI for tests: no daemon required.
type fakeAPI struct {
	// Provider surface.
	created    *container.Config
	hostCfg    *container.HostConfig
	createName string
	started    []string
	removed    []string
	removedF   []bool
	listArgs   filters.Args
	listResult []container.Summary

	// Inspect behaviour (container readiness).
	inspectRunning bool
	inspectErr     error

	// Exec behaviour.
	mu        sync.Mutex
	execCalls []execCall
	stdin     bytes.Buffer // captures bytes written to the first exec's stdin
	stdinSeen bool
	exitCode  int   // exit code reported by ContainerExecInspect
	stillRun  bool  // report exec still running (error path)
	attachErr error // error returned by ContainerExecAttach
}

func (f *fakeAPI) ContainerCreate(_ context.Context, config *container.Config, hostConfig *container.HostConfig, name string) (string, error) {
	f.created = config
	f.hostCfg = hostConfig
	f.createName = name
	return "container123", nil
}

func (f *fakeAPI) ContainerStart(_ context.Context, id string) error {
	f.started = append(f.started, id)
	return nil
}

func (f *fakeAPI) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	if f.inspectErr != nil {
		return container.InspectResponse{}, f.inspectErr
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:    id,
			Name:  "/worker-1",
			State: &container.State{Running: f.inspectRunning},
		},
		Config: &container.Config{Labels: map[string]string{tagLabel: testTag}},
	}, nil
}

func (f *fakeAPI) ContainerRemove(_ context.Context, id string, force bool) error {
	f.removed = append(f.removed, id)
	f.removedF = append(f.removedF, force)
	return nil
}

func (f *fakeAPI) ContainerList(_ context.Context, args filters.Args) ([]container.Summary, error) {
	f.listArgs = args
	return f.listResult, nil
}

func (f *fakeAPI) ContainerExecCreate(_ context.Context, id string, opts container.ExecOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls = append(f.execCalls, execCall{id: id, opts: opts})
	return "exec123", nil
}

func (f *fakeAPI) ContainerExecAttach(_ context.Context, _ string) (types.HijackedResponse, error) {
	if f.attachErr != nil {
		return types.HijackedResponse{}, f.attachErr
	}
	// Read side is empty (immediate EOF on the multiplexed stream); write side
	// captures stdin into f.stdin.
	conn := &fakeConn{out: &f.stdin, capture: f}
	return types.NewHijackedResponse(conn, "application/vnd.docker.multiplexed-stream"), nil
}

func (f *fakeAPI) ContainerExecInspect(_ context.Context, _ string) (container.ExecInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return container.ExecInspect{Running: f.stillRun, ExitCode: f.exitCode}, nil
}

func (f *fakeAPI) firstStdin() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stdin.String()
}

// fakeConn is a net.Conn whose reads return EOF immediately and whose writes
// are captured. It records that stdin was written so tests can assert the
// token write path.
type fakeConn struct {
	out     *bytes.Buffer
	capture *fakeAPI
}

func (c *fakeConn) Read(_ []byte) (int, error) { return 0, io.EOF }

func (c *fakeConn) Write(p []byte) (int, error) {
	c.capture.mu.Lock()
	defer c.capture.mu.Unlock()
	c.capture.stdinSeen = true
	return c.out.Write(p)
}

func (c *fakeConn) Close() error                       { return nil }
func (c *fakeConn) CloseWrite() error                  { return nil }
func (c *fakeConn) LocalAddr() net.Addr                { return fakeAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr               { return fakeAddr{} }
func (c *fakeConn) SetDeadline(_ time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(_ time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }
