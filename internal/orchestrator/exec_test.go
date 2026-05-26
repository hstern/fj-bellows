package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/forgejo"
	omock "github.com/hstern/fj-bellows/internal/orchestrator/mock"
	"github.com/hstern/fj-bellows/internal/provider"
	pmock "github.com/hstern/fj-bellows/internal/provider/mock"
)

// TestExecOnWorker_RejectsOversizedCommand pins the 64 KiB cap. The
// pool is empty so a too-short command would fail with "not in pool";
// the size check must fire first.
func TestExecOnWorker_RejectsOversizedCommand(t *testing.T) {
	o := New(baseConfig(), nil, nil, &omock.Dispatcher{}, nil)
	big := strings.Repeat("a", execCommandLimit+1)
	_, err := o.ExecOnWorker(t.Context(), "ghost", big)
	if err == nil || !strings.Contains(err.Error(), "command too long") {
		t.Fatalf("expected command-too-long error; got %v", err)
	}
}

// TestExecOnWorker_NonSSHDispatcherErrors pins the dispatcher gate:
// when the configured dispatcher isn't *SSHDispatcher (today: docker
// provider), ExecOnWorker returns ErrExecNotSupported. The plain
// orchestrator mock.Dispatcher is exactly that case.
func TestExecOnWorker_NonSSHDispatcherErrors(t *testing.T) {
	o := New(baseConfig(), nil, nil, &omock.Dispatcher{}, nil)
	_, err := o.ExecOnWorker(t.Context(), "100", "true")
	if !errors.Is(err, ErrExecNotSupported) {
		t.Fatalf("expected ErrExecNotSupported; got %v", err)
	}
}

// TestExecOnWorker_UnknownInstance pins the not-found path. The
// dispatcher is *SSHDispatcher (so the type check passes) but no node
// with the requested id exists in the pool.
func TestExecOnWorker_UnknownInstance(t *testing.T) {
	o := New(baseConfig(), nil, nil, &SSHDispatcher{}, nil)
	_, err := o.ExecOnWorker(t.Context(), "ghost", "true")
	if err == nil || !strings.Contains(err.Error(), "not in pool") {
		t.Fatalf("expected not-in-pool error; got %v", err)
	}
}

// TestExecOnWorker_WrongStateRejected pins that provisioning /
// removing / draining nodes are refused. Only idle and busy are
// runnable.
func TestExecOnWorker_WrongStateRejected(t *testing.T) {
	prov := &pmock.Provider{
		ListFn: func(_ context.Context, _ string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "100", IPv4: testIP, CreatedAt: time.Now()}}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) { return nil, nil },
	}
	o := New(baseConfig(), prov, jobs, &SSHDispatcher{}, nil)
	// Seed a provisioning node directly so we don't need the reconcile
	// loop running.
	o.pool.Put(&Node{InstanceID: "100", State: StateProvisioning, IP: testIP})
	_, err := o.ExecOnWorker(t.Context(), "100", "true")
	if err == nil || !strings.Contains(err.Error(), "exec requires idle or busy") {
		t.Fatalf("expected wrong-state error; got %v", err)
	}
}

// TestCappedBuffer_TracksTruncation pins the per-stream output cap +
// the "original byte count when truncation happened" contract.
func TestCappedBuffer_TracksTruncation(t *testing.T) {
	c := &cappedBuffer{limit: 8}
	// Under-limit: original() must be zero.
	if _, err := c.Write([]byte("abc")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := c.original(); got != 0 {
		t.Fatalf("untruncated original(): want 0 got %d", got)
	}
	// Cross the limit; bytes captured stay capped at limit.
	if _, err := c.Write([]byte("defghijklmnop")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := len(c.Bytes()); got != 8 {
		t.Fatalf("captured len: want 8 got %d", got)
	}
	if got := c.original(); got != 16 {
		t.Fatalf("original(): want 16 got %d", got)
	}
}
