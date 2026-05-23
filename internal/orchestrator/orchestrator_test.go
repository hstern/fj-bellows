package orchestrator

import (
	"context"
	"strconv"
	"strings"
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

// trackingProvider builds a mock provider whose List reports every instance it
// has provisioned (plus any seeded adopted instances), mirroring a real provider
// that returns the VMs it created. Each Provision hands out a distinct ID. This
// lets a second Reconcile see the warm pool instead of treating it as vanished.
func trackingProvider(seed ...provider.Instance) *pmock.Provider {
	var mu sync.Mutex
	var n int
	live := append([]provider.Instance(nil), seed...)
	p := &pmock.Provider{}
	p.ProvisionFn = func(_ context.Context, _ provider.Spec) (provider.Instance, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		inst := provider.Instance{
			ID:        "vm-" + strconv.Itoa(n),
			IPv4:      "10.0.0." + strconv.Itoa(n),
			CreatedAt: time.Now(),
		}
		live = append(live, inst)
		return inst, nil
	}
	p.ListFn = func(context.Context, string) ([]provider.Instance, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]provider.Instance(nil), live...), nil
	}
	return p
}

func nUbuntuJobs(n int) []forgejo.WaitingJob {
	jobs := make([]forgejo.WaitingJob, 0, n)
	for i := range n {
		jobs = append(jobs, forgejo.WaitingJob{
			Handle: "h" + strconv.Itoa(i),
			Labels: []string{labelUbuntu},
		})
	}
	return jobs
}

// TestReconcileScalesToMaxOnEmptyPool covers MaxScale=3 with an empty pool and
// 3 serviceable jobs: exactly 3 provisions, never more across extra reconciles,
// and the pool converges to 3 idle nodes.
func TestReconcileScalesToMaxOnEmptyPool(t *testing.T) {
	prov := trackingProvider()
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return nUbuntuJobs(3), nil
		},
	}
	cfg := baseConfig()
	cfg.MaxScale = 3
	o := New(cfg, prov, jobs, &omock.Dispatcher{}, nil)

	o.Reconcile(context.Background())
	waitFor(t, "pool reaches 3 idle nodes", func() bool {
		return len(o.pool.ByState(StateIdle)) == 3
	})

	// Extra reconciles must not over-provision: the pool is full and the jobs
	// are still "waiting" in the mock, but there is no idle headroom left.
	o.Reconcile(context.Background())
	o.Reconcile(context.Background())
	time.Sleep(50 * time.Millisecond)

	if prov.ProvisionCount() != 3 {
		t.Errorf("ProvisionCount = %d, want 3 (max_scale=3)", prov.ProvisionCount())
	}
	if o.pool.Len() != 3 {
		t.Errorf("pool.Len = %d, want 3", o.pool.Len())
	}
}

// TestReconcileDispatchesToAllAdoptedIdle covers MaxScale=3 with 3 already-idle
// adopted instances and 3 jobs: 3 dispatches, 0 provisions.
func TestReconcileDispatchesToAllAdoptedIdle(t *testing.T) {
	prov := trackingProvider(
		provider.Instance{ID: "a", IPv4: "10.0.0.1", CreatedAt: time.Now()},
		provider.Instance{ID: "b", IPv4: "10.0.0.2", CreatedAt: time.Now()},
		provider.Instance{ID: "c", IPv4: "10.0.0.3", CreatedAt: time.Now()},
	)
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return nUbuntuJobs(3), nil
		},
	}
	disp := &omock.Dispatcher{}
	cfg := baseConfig()
	cfg.MaxScale = 3
	o := New(cfg, prov, jobs, disp, nil)

	o.Reconcile(context.Background())
	waitFor(t, "all three jobs dispatched", func() bool { return disp.RunCount() == 3 })
	time.Sleep(50 * time.Millisecond)

	if prov.ProvisionCount() != 0 {
		t.Errorf("ProvisionCount = %d, want 0 (reuse warm nodes)", prov.ProvisionCount())
	}
	if disp.RunCount() != 3 {
		t.Errorf("RunCount = %d, want 3", disp.RunCount())
	}
}

// TestReconcileMixDispatchAndProvision covers MaxScale=3 with 1 idle node and 3
// jobs: 1 dispatch reuses the warm node, 2 provisions cover the rest, and the
// total never exceeds MaxScale.
func TestReconcileMixDispatchAndProvision(t *testing.T) {
	prov := trackingProvider(provider.Instance{ID: "warm", IPv4: "10.0.0.9", CreatedAt: time.Now()})
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return nUbuntuJobs(3), nil
		},
	}
	disp := &omock.Dispatcher{}
	cfg := baseConfig()
	cfg.MaxScale = 3
	o := New(cfg, prov, jobs, disp, nil)

	o.Reconcile(context.Background())
	waitFor(t, "one dispatch and two provisions", func() bool {
		return disp.RunCount() == 1 && prov.ProvisionCount() == 2
	})
	time.Sleep(50 * time.Millisecond)

	if disp.RunCount() != 1 {
		t.Errorf("RunCount = %d, want 1", disp.RunCount())
	}
	if prov.ProvisionCount() != 2 {
		t.Errorf("ProvisionCount = %d, want 2 (1 warm + 2 new = max 3)", prov.ProvisionCount())
	}
}

// TestReconcileTearsDownAllDueIdleNodes covers that every idle node past its
// kill mark is destroyed in a single reconcile, not just the first.
func TestReconcileTearsDownAllDueIdleNodes(t *testing.T) {
	created := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{
				{ID: "x", CreatedAt: created},
				{ID: "y", CreatedAt: created},
				{ID: "z", CreatedAt: created},
			}, nil
		},
	}
	cfg := baseConfig()
	cfg.MaxScale = 3
	cfg.Teardown = TeardownPolicy{Model: provider.BillingHourlyRoundUp, HourMargin: 5 * time.Minute}
	o := New(cfg, prov, &omock.JobSource{}, &omock.Dispatcher{}, nil)
	o.now = func() time.Time { return created.Add(56 * time.Minute) }

	o.Reconcile(context.Background())
	waitFor(t, "all three idle nodes destroyed", func() bool { return prov.DestroyCount() == 3 })
	waitFor(t, "pool drained", func() bool { return o.pool.Len() == 0 })
}

// TestPendingPreventsOverProvision covers the pending counter: with provisions
// blocked in flight, a second reconcile must not exceed MaxScale because pending
// is incremented synchronously before each provision goroutine starts.
func TestPendingPreventsOverProvision(t *testing.T) {
	release := make(chan struct{})
	var inFlight, assigned atomic.Int32
	prov := &pmock.Provider{
		ProvisionFn: func(_ context.Context, _ provider.Spec) (provider.Instance, error) {
			id := int(assigned.Add(1)) // distinct ID per call, taken before blocking
			inFlight.Add(1)
			<-release // block until the test releases all provisions
			return provider.Instance{
				ID:        "vm-" + strconv.Itoa(id),
				IPv4:      "10.0.0." + strconv.Itoa(id),
				CreatedAt: time.Now(),
			}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return nUbuntuJobs(3), nil
		},
	}
	cfg := baseConfig()
	cfg.MaxScale = 3
	o := New(cfg, prov, jobs, &omock.Dispatcher{}, nil)

	// First reconcile starts up to MaxScale provisions, all blocked in Provision.
	o.Reconcile(context.Background())
	waitFor(t, "three provisions in flight", func() bool { return inFlight.Load() == 3 })

	// Second reconcile while provisions are blocked: pending == 3 leaves zero
	// headroom, so no further provision must start.
	o.Reconcile(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := prov.ProvisionCount(); got != 3 {
		t.Errorf("ProvisionCount = %d, want 3 (pending must cap concurrent reconciles)", got)
	}

	close(release) // unblock; let the goroutines finish so goleak stays clean
	waitFor(t, "pool reaches 3 idle nodes", func() bool {
		return len(o.pool.ByState(StateIdle)) == 3
	})
	if prov.ProvisionCount() != 3 {
		t.Errorf("ProvisionCount = %d, want 3 after release", prov.ProvisionCount())
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
		RunJobFn: func(_ context.Context, _, _ string, _ forgejo.Registration, _ forgejo.WaitingJob) error {
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
		RunJobFn: func(ctx context.Context, _, _ string, _ forgejo.Registration, _ forgejo.WaitingJob) error {
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

// TestProvisionSeedsHostKeyPin covers that when the dispatcher implements
// HostKeyPinner, provisioning a VM seeds a host-key pin for the provisioned IP.
func TestProvisionSeedsHostKeyPin(t *testing.T) {
	var gotUserData string
	prov := &pmock.Provider{
		ProvisionFn: func(_ context.Context, spec provider.Spec) (provider.Instance, error) {
			gotUserData = spec.UserData
			return provider.Instance{ID: "100", IPv4: "10.0.0.42", CreatedAt: time.Now()}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return []forgejo.WaitingJob{{Handle: "h1", Labels: []string{labelUbuntu}}}, nil
		},
	}
	disp := &omock.PinningDispatcher{}
	o := New(baseConfig(), prov, jobs, disp, nil)

	o.Reconcile(context.Background())

	waitFor(t, "host key pinned for provisioned IP", func() bool {
		_, ok := disp.PinnedKey("10.0.0.42")
		return ok
	})
	if disp.PinCount() != 1 {
		t.Errorf("PinCount = %d, want 1", disp.PinCount())
	}
	// The injected cloud-init must carry the host key install for the worker.
	if !strings.Contains(gotUserData, "/etc/ssh/ssh_host_ed25519_key") {
		t.Error("cloud-init missing injected host key for a pinning dispatcher")
	}
}

// TestProvisionWithoutPinnerSkipsHostKey covers that a non-pinning dispatcher
// neither seeds a pin nor gets a host key injected into cloud-init.
func TestProvisionWithoutPinnerSkipsHostKey(t *testing.T) {
	var gotUserData string
	prov := &pmock.Provider{
		ProvisionFn: func(_ context.Context, spec provider.Spec) (provider.Instance, error) {
			gotUserData = spec.UserData
			return provider.Instance{ID: "100", IPv4: "10.0.0.43", CreatedAt: time.Now()}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return []forgejo.WaitingJob{{Handle: "h1", Labels: []string{labelUbuntu}}}, nil
		},
	}
	disp := &omock.Dispatcher{} // plain dispatcher: not a HostKeyPinner
	o := New(baseConfig(), prov, jobs, disp, nil)

	o.Reconcile(context.Background())

	waitFor(t, "node becomes idle after provision", func() bool {
		return len(o.pool.ByState(StateIdle)) == 1
	})
	if strings.Contains(gotUserData, "ssh_host_ed25519_key") {
		t.Error("cloud-init injected a host key for a non-pinning dispatcher")
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

// TestReconcileCountsProvisioningAsFutureCapacity is the regression test for
// #32. With a slow boot (boot_time >> poll_interval), nodes sit in
// StateProvisioning across multiple reconciles while the same waiting jobs are
// still on the queue. The fix credits StateProvisioning + pending against
// unmet demand; without it, every tick would re-stamp the same N provisions
// until MaxScale capped the runaway.
func TestReconcileCountsProvisioningAsFutureCapacity(t *testing.T) {
	release := make(chan struct{})
	prov := trackingProvider()
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			// Forgejo still reports the same jobs as waiting because no runner
			// has registered yet (the boot is in flight).
			return nUbuntuJobs(2), nil
		},
	}
	// Dispatcher blocks WaitReady, freezing nodes in StateProvisioning until
	// the test releases them. This mirrors a real cloud where Provision returns
	// the instance ID in seconds but SSH readiness lags by ~60-90s.
	disp := &omock.Dispatcher{
		WaitReadyFn: func(context.Context, string, string) error {
			<-release
			return nil
		},
	}
	cfg := baseConfig()
	cfg.MaxScale = 8 // generous headroom so MaxScale cannot mask the bug
	o := New(cfg, prov, jobs, disp, nil)

	// First reconcile: 2 jobs, 0 idle -> 2 provisions, both stuck in WaitReady.
	o.Reconcile(context.Background())
	waitFor(t, "two nodes in StateProvisioning", func() bool {
		return len(o.pool.ByState(StateProvisioning)) == 2
	})

	// Subsequent reconciles must not stamp out more VMs: the 2 booting nodes
	// are credited against the 2 still-waiting jobs.
	o.Reconcile(context.Background())
	o.Reconcile(context.Background())
	o.Reconcile(context.Background())
	time.Sleep(50 * time.Millisecond)

	if got := prov.ProvisionCount(); got != 2 {
		t.Errorf("ProvisionCount = %d, want 2 (StateProvisioning must credit future capacity; pre-fix would be 8 = MaxScale)", got)
	}

	// Release and let goroutines finish so goleak stays clean.
	close(release)
	waitFor(t, "pool reaches 2 idle nodes", func() bool {
		return len(o.pool.ByState(StateIdle)) == 2
	})
}

// TestReconcileSubtractsProvisioningFromNeed exercises the credit math with a
// mix: 5 jobs, 1 idle, 2 already-provisioning. Expected new provisions: 2
// (5 - 1 idle assigned in the loop - 2 provisioning credited as future).
func TestReconcileSubtractsProvisioningFromNeed(t *testing.T) {
	prov := trackingProvider(
		// The idle node is reported by List so syncPool adopts it.
		provider.Instance{ID: "idle1", IPv4: "10.0.0.1", CreatedAt: time.Now()},
	)
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return nUbuntuJobs(5), nil
		},
	}
	disp := &omock.Dispatcher{}
	cfg := baseConfig()
	cfg.MaxScale = 10
	o := New(cfg, prov, jobs, disp, nil)
	// Pre-seed 2 nodes already booting from a prior tick. They are not in the
	// provider's List output (we want syncPool to leave them alone via the
	// StateProvisioning carve-out rather than re-adopt them).
	o.pool.Put(&Node{InstanceID: "prov1", State: StateProvisioning, IP: "10.0.0.2"})
	o.pool.Put(&Node{InstanceID: "prov2", State: StateProvisioning, IP: "10.0.0.3"})

	o.Reconcile(context.Background())
	waitFor(t, "1 dispatch and 2 new provisions", func() bool {
		return disp.RunCount() == 1 && prov.ProvisionCount() == 2
	})
	time.Sleep(50 * time.Millisecond)

	if disp.RunCount() != 1 {
		t.Errorf("RunCount = %d, want 1 (one job served by the idle node)", disp.RunCount())
	}
	if got := prov.ProvisionCount(); got != 2 {
		t.Errorf("ProvisionCount = %d, want 2 (5 jobs - 1 idle - 2 provisioning)", got)
	}
}
