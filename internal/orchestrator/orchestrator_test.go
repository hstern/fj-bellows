package orchestrator

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/forgejo"
	omock "github.com/hstern/fj-bellows/internal/orchestrator/mock"
	"github.com/hstern/fj-bellows/internal/provider"
	pmock "github.com/hstern/fj-bellows/internal/provider/mock"
)

const labelUbuntu = "ubuntu-latest"

func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within timeout: %s", msg)
}

func baseConfig() Config {
	return Config{
		Tag:           "fj-bellows",
		MaxScale:      1,
		Labels:        []string{labelUbuntu},
		PollInterval:  time.Hour,
		RunnerVersion: "1.0.0",
		Teardown:      TeardownPolicy{Model: provider.BillingPerSecond, IdleTimeout: 5 * time.Minute},
	}
}

func TestReconcileProvisionsForWaitingJob(t *testing.T) {
	prov := &pmock.Provider{
		ProvisionFn: func(_ context.Context, _ provider.Spec) (provider.Instance, error) {
			return provider.Instance{ID: "100", IPv4: "10.0.0.1", CreatedAt: time.Now()}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return []forgejo.WaitingJob{{Handle: "h1", Labels: []string{labelUbuntu}}}, nil
		},
	}
	disp := &omock.Dispatcher{}
	o := New(baseConfig(), prov, jobs, disp, nil)

	o.Reconcile(context.Background())

	waitFor(t, "node becomes idle after provision", func() bool {
		idle := o.pool.ByState(StateIdle)
		return len(idle) == 1 && idle[0].InstanceID == "100"
	})
	if prov.ProvisionCount() != 1 {
		t.Errorf("ProvisionCount = %d, want 1", prov.ProvisionCount())
	}
}

func TestReconcileDispatchesToIdleNode(t *testing.T) {
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "1", IPv4: "10.0.0.5", CreatedAt: time.Now()}}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return []forgejo.WaitingJob{{Handle: "h1", Labels: []string{labelUbuntu}}}, nil
		},
	}
	disp := &omock.Dispatcher{}
	o := New(baseConfig(), prov, jobs, disp, nil)

	o.Reconcile(context.Background())

	waitFor(t, "job dispatched to adopted idle node", func() bool {
		return disp.RunCount() == 1
	})
	if prov.ProvisionCount() != 0 {
		t.Errorf("should reuse warm node, not provision; ProvisionCount = %d", prov.ProvisionCount())
	}
	if jobs.RegisterCount() != 1 {
		t.Errorf("RegisterCount = %d, want 1", jobs.RegisterCount())
	}
}

func TestReconcileRespectsMaxScale(t *testing.T) {
	prov := &pmock.Provider{
		ProvisionFn: func(_ context.Context, _ provider.Spec) (provider.Instance, error) {
			return provider.Instance{ID: "200", IPv4: "10.0.0.2", CreatedAt: time.Now()}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return []forgejo.WaitingJob{
				{Handle: "h1", Labels: []string{labelUbuntu}},
				{Handle: "h2", Labels: []string{labelUbuntu}},
			}, nil
		},
	}
	o := New(baseConfig(), prov, jobs, &omock.Dispatcher{}, nil)

	o.Reconcile(context.Background())
	waitFor(t, "one node provisioned", func() bool { return o.pool.Len() == 1 })
	// give any erroneous extra provision a chance to land
	time.Sleep(50 * time.Millisecond)
	if prov.ProvisionCount() != 1 {
		t.Errorf("ProvisionCount = %d, want 1 (max_scale=1)", prov.ProvisionCount())
	}
}

func TestReconcileTeardownHourlyAtFiftyFive(t *testing.T) {
	created := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "9", IPv4: "10.0.0.9", CreatedAt: created}}, nil
		},
	}
	jobs := &omock.JobSource{} // no waiting jobs
	cfg := baseConfig()
	cfg.Teardown = TeardownPolicy{Model: provider.BillingHourlyRoundUp, HourMargin: 5 * time.Minute}
	o := New(cfg, prov, jobs, &omock.Dispatcher{}, nil)
	o.now = func() time.Time { return created.Add(56 * time.Minute) }

	o.Reconcile(context.Background())
	waitFor(t, "idle node destroyed at :55", func() bool { return prov.DestroyCount() == 1 })
	waitFor(t, "destroyed node removed from pool", func() bool { return o.pool.Len() == 0 })
}

func TestReconcileNoTeardownBeforeFiftyFive(t *testing.T) {
	created := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "9", CreatedAt: created}}, nil
		},
	}
	cfg := baseConfig()
	cfg.Teardown = TeardownPolicy{Model: provider.BillingHourlyRoundUp, HourMargin: 5 * time.Minute}
	o := New(cfg, prov, &omock.JobSource{}, &omock.Dispatcher{}, nil)
	o.now = func() time.Time { return created.Add(30 * time.Minute) }

	o.Reconcile(context.Background())
	time.Sleep(50 * time.Millisecond)
	if prov.DestroyCount() != 0 {
		t.Errorf("DestroyCount = %d, want 0 (warm-hold)", prov.DestroyCount())
	}
}

func TestSyncPoolDropsVanishedButKeepsProvisioning(t *testing.T) {
	var listCall atomic.Int32
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			if listCall.Add(1) == 1 {
				return []provider.Instance{{ID: "1", CreatedAt: time.Now()}}, nil
			}
			return nil, nil // vanished on subsequent ticks
		},
	}
	o := New(baseConfig(), prov, &omock.JobSource{}, &omock.Dispatcher{}, nil)

	o.Reconcile(context.Background()) // adopts "1"
	waitFor(t, "instance adopted", func() bool { _, ok := o.pool.Get("1"); return ok })

	// A provisioning node not yet visible in List must survive the sweep.
	o.pool.Put(&Node{InstanceID: "prov", State: StateProvisioning})
	o.Reconcile(context.Background()) // "1" vanishes, "prov" stays

	if _, ok := o.pool.Get("1"); ok {
		t.Error("vanished instance was not dropped")
	}
	if _, ok := o.pool.Get("prov"); !ok {
		t.Error("provisioning node was wrongly dropped")
	}
}

func TestReapZombieRunnersAfterTwoTicks(t *testing.T) {
	prov := &pmock.Provider{} // no instances
	jobs := &omock.JobSource{
		ListRunnersFn: func(context.Context) ([]forgejo.Runner, error) {
			return []forgejo.Runner{
				{ID: 7, UUID: "u-7", Name: "fj-bellows-dead", Status: "offline"}, // ours, zombie
				{ID: 9, UUID: "u-9", Name: "some-other-runner"},                  // not ours
			}, nil
		},
	}
	o := New(baseConfig(), prov, jobs, &omock.Dispatcher{}, nil)

	o.Reconcile(context.Background()) // first sighting: no delete yet
	if jobs.DeleteCount() != 0 {
		t.Fatalf("deleted on first sighting: %d", jobs.DeleteCount())
	}
	o.Reconcile(context.Background()) // second sighting: reap
	if jobs.DeleteCount() != 1 || jobs.DeleteCalls[0] != 7 {
		t.Fatalf("expected to reap runner 7, got DeleteCalls=%v", jobs.DeleteCalls)
	}
}

func TestReapSkipsActiveRunner(t *testing.T) {
	prov := &pmock.Provider{}
	jobs := &omock.JobSource{
		ListRunnersFn: func(context.Context) ([]forgejo.Runner, error) {
			return []forgejo.Runner{{ID: 1, UUID: "live", Name: "fj-bellows-live"}}, nil
		},
	}
	o := New(baseConfig(), prov, jobs, &omock.Dispatcher{}, nil)
	o.addActive("live") // currently running a job

	o.Reconcile(context.Background())
	o.Reconcile(context.Background())
	if jobs.DeleteCount() != 0 {
		t.Errorf("reaped an active runner: %v", jobs.DeleteCalls)
	}
}

func TestRunDrainsInFlightJob(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "1", IPv4: "10.0.0.5", CreatedAt: time.Now()}}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return []forgejo.WaitingJob{{Handle: "h1", Labels: []string{labelUbuntu}}}, nil
		},
	}
	var startedOnce sync.Once
	disp := &omock.Dispatcher{
		RunJobFn: func(_ context.Context, _ string, _ forgejo.Registration, _ forgejo.WaitingJob) error {
			startedOnce.Do(func() { close(started) })
			<-release // block until the test releases (simulates a long job)
			return nil
		},
	}
	cfg := baseConfig()
	cfg.PollInterval = 10 * time.Millisecond
	cfg.DrainOnShutdown = true
	o := New(cfg, prov, jobs, disp, nil)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = o.Run(ctx); close(runDone) }()

	<-started // a job is in flight
	cancel()  // signal shutdown

	select {
	case <-runDone:
		t.Fatal("Run returned before the in-flight job finished (did not drain)")
	case <-time.After(100 * time.Millisecond):
	}

	close(release) // let the job finish
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the job drained")
	}
}

func TestRunInterruptsWhenNoDrain(t *testing.T) {
	started := make(chan struct{})
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "1", IPv4: "10.0.0.5", CreatedAt: time.Now()}}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return []forgejo.WaitingJob{{Handle: "h1", Labels: []string{labelUbuntu}}}, nil
		},
	}
	var startedOnce sync.Once
	disp := &omock.Dispatcher{
		RunJobFn: func(ctx context.Context, _ string, _ forgejo.Registration, _ forgejo.WaitingJob) error {
			startedOnce.Do(func() { close(started) })
			<-ctx.Done() // respects cancellation, like the real SSH dispatcher
			return ctx.Err()
		},
	}
	cfg := baseConfig()
	cfg.PollInterval = 10 * time.Millisecond
	cfg.DrainOnShutdown = false
	o := New(cfg, prov, jobs, disp, nil)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = o.Run(ctx); close(runDone) }()

	<-started
	cancel() // interrupt immediately

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly when interrupting in-flight jobs")
	}
}

func TestRunDestroyOnExit(t *testing.T) {
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "1", CreatedAt: time.Now()}}, nil
		},
	}
	cfg := baseConfig()
	cfg.PollInterval = 10 * time.Millisecond
	cfg.DestroyOnExit = true
	o := New(cfg, prov, &omock.JobSource{}, &omock.Dispatcher{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = o.Run(ctx); close(runDone) }()

	waitFor(t, "instance adopted", func() bool { return o.pool.Len() == 1 })
	cancel()
	<-runDone
	if prov.DestroyCount() != 1 {
		t.Errorf("DestroyCount = %d, want 1 (destroy-on-exit)", prov.DestroyCount())
	}
}

func TestFilterServiceable(t *testing.T) {
	labels := []string{labelUbuntu, "amd64"}
	jobs := []forgejo.WaitingJob{
		{Handle: "ok", Labels: []string{labelUbuntu}},
		{Handle: "ok2", Labels: []string{labelUbuntu, "amd64"}},
		{Handle: "nolabels"},
		{Handle: "nope", Labels: []string{"windows"}},
	}
	got := filterServiceable(jobs, labels)
	if len(got) != 3 {
		t.Fatalf("got %d serviceable jobs, want 3: %+v", len(got), got)
	}
	for _, j := range got {
		if j.Handle == "nope" {
			t.Error("unserviceable job leaked through")
		}
	}
}
