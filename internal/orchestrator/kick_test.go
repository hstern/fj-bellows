package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/forgejo"
	omock "github.com/hstern/fj-bellows/internal/orchestrator/mock"
	"github.com/hstern/fj-bellows/internal/provider"
	pmock "github.com/hstern/fj-bellows/internal/provider/mock"
)

// TestKickDrivesSynchronousReconcile asserts the control plane's Kick path
// drives one reconcile from the Run goroutine and returns the ReconcileResult.
// Single-writer property: kick + ticker share the same select.
func TestKickDrivesSynchronousReconcile(t *testing.T) {
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "1", IPv4: "10.0.0.11", CreatedAt: time.Now()}}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return nil, nil
		},
	}
	disp := &omock.Dispatcher{}
	o := New(baseConfig(), prov, jobs, disp, nil)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = o.Run(runCtx); close(done) }()

	// Wait for the initial reconcile to have adopted the seeded instance.
	waitFor(t, "initial reconcile adopted", func() bool {
		return o.pool.Len() == 1
	})

	r, err := o.Kick(t.Context())
	if err != nil {
		t.Fatalf("Kick: %v", err)
	}
	// The second reconcile sees the already-adopted node so Adopted should
	// be 0; Dropped also 0; Dispatched/Provisioned 0 (no waiting jobs).
	if r.Adopted != 0 || r.Dispatched != 0 || r.Provisioned != 0 {
		t.Fatalf("second tick counts: %+v", r)
	}

	cancel()
	<-done
}

// TestSubscribeReceivesEmittedEvents asserts emit→Subscribe end-to-end.
func TestSubscribeReceivesEmittedEvents(t *testing.T) {
	o := New(baseConfig(), nil, nil, nil, nil)
	ch, cancel := o.Subscribe()
	defer cancel()

	o.emit("worker_busy", map[string]string{"id": "vm-1", "handle": "h1"})

	select {
	case ev := <-ch:
		if ev.Type != "worker_busy" || ev.Attrs["id"] != "vm-1" {
			t.Fatalf("event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
}

// TestKickWithoutRunningOrchestrator_ReturnsError pins that Kick fails fast
// when the Run goroutine isn't there to service the channel.
func TestKickWithoutRunningOrchestrator_ReturnsError(t *testing.T) {
	o := New(baseConfig(), nil, nil, nil, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := o.Kick(ctx)
	if err == nil {
		t.Fatal("Kick without Run should return an error")
	}
}
