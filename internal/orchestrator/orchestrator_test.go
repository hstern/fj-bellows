package orchestrator

import (
	"context"
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
