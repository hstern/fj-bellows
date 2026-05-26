package orchestrator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/forgejo"
	omock "github.com/hstern/fj-bellows/internal/orchestrator/mock"
	"github.com/hstern/fj-bellows/internal/provider"
	pmock "github.com/hstern/fj-bellows/internal/provider/mock"
)

// runOrchestrator starts the orchestrator's Run loop with a noop ticker
// (PollInterval long enough that only the kick path drives reconciles).
// Cleanup blocks the goroutine until ctx cancels.
func runOrchestrator(t *testing.T, o *Orchestrator) {
	t.Helper()
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = o.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// TestForceReap_DestroysAndDropsFromPool pins the happy path: an idle node
// in the pool is destroyed and dropped synchronously from the caller's
// perspective.
func TestForceReap_DestroysAndDropsFromPool(t *testing.T) {
	var destroyed atomic.Int32
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "100", IPv4: testIP, CreatedAt: time.Now()}}, nil
		},
		DestroyFn: func(_ context.Context, _ string) error {
			destroyed.Add(1)
			return nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) { return nil, nil },
	}
	o := New(baseConfig(), prov, jobs, &omock.Dispatcher{}, nil)
	runOrchestrator(t, o)

	waitFor(t, "initial adoption", func() bool { return o.pool.Len() == 1 })

	ctx := WithAuditCaller(t.Context(), "peer=127.0.0.1:12345")
	if err := o.ForceReap(ctx, "100"); err != nil {
		t.Fatalf("ForceReap: %v", err)
	}
	if destroyed.Load() != 1 {
		t.Fatalf("Destroy called %d times; want 1", destroyed.Load())
	}
	if _, ok := o.pool.Get("100"); ok {
		t.Fatal("node should have been dropped from pool")
	}
}

// TestForceReap_UnknownInstanceErrors pins the not-found path.
func TestForceReap_UnknownInstanceErrors(t *testing.T) {
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) { return nil, nil },
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) { return nil, nil },
	}
	o := New(baseConfig(), prov, jobs, &omock.Dispatcher{}, nil)
	runOrchestrator(t, o)

	err := o.ForceReap(t.Context(), "ghost")
	if err == nil || err.Error() == "" {
		t.Fatalf("expected not-in-pool error; got %v", err)
	}
}

// TestForceReap_DestroyFailureLeavesNodeIdle pins the recovery path: when
// provider.Destroy fails, the node is left Idle so the next teardown tick
// (or another force-reap) can retry.
func TestForceReap_DestroyFailureLeavesNodeIdle(t *testing.T) {
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "100", IPv4: testIP, CreatedAt: time.Now()}}, nil
		},
		DestroyFn: func(context.Context, string) error { return errors.New("api down") },
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) { return nil, nil },
	}
	o := New(baseConfig(), prov, jobs, &omock.Dispatcher{}, nil)
	runOrchestrator(t, o)

	waitFor(t, "initial adoption", func() bool { return o.pool.Len() == 1 })

	err := o.ForceReap(t.Context(), "100")
	if err == nil {
		t.Fatal("expected Destroy failure to surface")
	}
	n, ok := o.pool.Get("100")
	if !ok {
		t.Fatal("node should still be in pool after failed destroy")
	}
	if n.State != StateIdle {
		t.Fatalf("state after failed force-reap: want %q got %q", StateIdle, n.State)
	}
}

// TestForceProvision_BypassesScaleMax pins that force-provision creates a
// worker even when the pool already holds scale.max nodes.
func TestForceProvision_BypassesScaleMax(t *testing.T) {
	var provisioned atomic.Int32
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "100", IPv4: testIP, CreatedAt: time.Now()}}, nil
		},
		ProvisionFn: func(_ context.Context, _ provider.Spec) (provider.Instance, error) {
			provisioned.Add(1)
			return provider.Instance{ID: "200", IPv4: "10.0.0.6", CreatedAt: time.Now()}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) { return nil, nil },
	}
	// baseConfig sets MaxScale = 1; pool will have one adopted node already.
	o := New(baseConfig(), prov, jobs, &omock.Dispatcher{}, nil)
	runOrchestrator(t, o)

	waitFor(t, "initial adoption", func() bool { return o.pool.Len() == 1 })

	id, err := o.ForceProvision(t.Context())
	if err != nil {
		t.Fatalf("ForceProvision: %v", err)
	}
	if id != "200" {
		t.Fatalf("ForceProvision returned id %q; want 200", id)
	}
	if provisioned.Load() != 1 {
		t.Fatalf("Provision called %d times; want 1", provisioned.Load())
	}
	// Pool now exceeds MaxScale — that's the bypass.
	if o.pool.Len() != 2 {
		t.Fatalf("pool size after force-provision: want 2 got %d", o.pool.Len())
	}
}

// TestForceProvision_PropagatesProvisionError pins that an immediate
// provider error surfaces synchronously to the RPC caller.
func TestForceProvision_PropagatesProvisionError(t *testing.T) {
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) { return nil, nil },
		ProvisionFn: func(context.Context, provider.Spec) (provider.Instance, error) {
			return provider.Instance{}, errors.New("out of quota")
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) { return nil, nil },
	}
	o := New(baseConfig(), prov, jobs, &omock.Dispatcher{}, nil)
	runOrchestrator(t, o)

	_, err := o.ForceProvision(t.Context())
	if err == nil {
		t.Fatal("expected Provision failure to surface")
	}
}

// TestForce_NoRun_ReturnsError pins that calling ForceReap / ForceProvision
// without an active Run goroutine fails fast rather than blocking.
func TestForce_NoRun_ReturnsError(t *testing.T) {
	o := New(baseConfig(), nil, nil, nil, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := o.ForceReap(ctx, "100"); err == nil {
		t.Fatal("ForceReap without Run should error")
	}
	if _, err := o.ForceProvision(ctx); err == nil {
		t.Fatal("ForceProvision without Run should error")
	}
}

// TestAuditCallerFromCtx covers the helper round-trip + the "loopback"
// fallback when nothing's set.
func TestAuditCallerFromCtx(t *testing.T) {
	if got := auditCallerFromCtx(context.Background()); got != "loopback" {
		t.Fatalf("bare context: want loopback got %q", got)
	}
	ctx := WithAuditCaller(context.Background(), "peer=1.2.3.4:55 token")
	if got := auditCallerFromCtx(ctx); got != "peer=1.2.3.4:55 token" {
		t.Fatalf("set: got %q", got)
	}
	// Empty caller passes through to the default.
	ctx2 := WithAuditCaller(context.Background(), "")
	if got := auditCallerFromCtx(ctx2); got != "loopback" {
		t.Fatalf("empty: want loopback got %q", got)
	}
}
