package orchestrator

import (
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/provider"
)

func TestPoolSetJob(t *testing.T) {
	p := NewPool()
	p.SetJob("missing", "should-not-panic") // no-op
	p.Put(&Node{InstanceID: "x", State: StateIdle})

	p.SetJob("x", "job-1")
	if got, _ := p.Get("x"); got.CurrentJob != "job-1" {
		t.Fatalf("CurrentJob: want job-1 got %q", got.CurrentJob)
	}

	p.SetJob("x", "")
	if got, _ := p.Get("x"); got.CurrentJob != "" {
		t.Fatalf("CurrentJob after clear: want empty got %q", got.CurrentJob)
	}
}

func TestPoolSnapshot_StringifiesState(t *testing.T) {
	o := &Orchestrator{
		pool: NewPool(),
		now:  time.Now,
	}
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	o.pool.Put(&Node{
		InstanceID: "linode-1",
		State:      StateBusy,
		IP:         "203.0.113.5",
		CreatedAt:  now.Add(-10 * time.Minute),
		LastBusy:   now,
		CurrentJob: "job-42",
	})
	o.pool.Put(&Node{
		InstanceID: "linode-2",
		State:      StateIdle,
	})

	got := o.PoolSnapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot length: want 2 got %d", len(got))
	}

	byID := map[string]WorkerView{}
	for _, w := range got {
		byID[w.InstanceID] = w
	}

	busy := byID["linode-1"]
	if busy.State != "busy" {
		t.Fatalf("state stringification: want busy got %q", busy.State)
	}
	if busy.CurrentJob != "job-42" {
		t.Fatalf("CurrentJob propagation: want job-42 got %q", busy.CurrentJob)
	}
	if !busy.CreatedAt.Equal(now.Add(-10 * time.Minute)) {
		t.Fatalf("CreatedAt: %v", busy.CreatedAt)
	}

	idle := byID["linode-2"]
	if idle.State != "idle" {
		t.Fatalf("idle state: want idle got %q", idle.State)
	}
	if idle.CurrentJob != "" {
		t.Fatalf("idle CurrentJob should be empty; got %q", idle.CurrentJob)
	}
}

// TestPoolSnapshot_BillingWindow covers FJB-30: PoolSnapshot pulls the
// billing-window timing from the configured TeardownPolicy so ListWorkers
// can surface it.
func TestPoolSnapshot_BillingWindow(t *testing.T) {
	created := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	now := created.Add(10 * time.Minute)
	o := &Orchestrator{
		pool: NewPool(),
		now:  func() time.Time { return now },
		cfg: Config{
			Teardown: TeardownPolicy{
				Model:       provider.BillingHourlyRoundUp,
				HourMargin:  5 * time.Minute,
				BillingHour: time.Hour,
			},
		},
	}
	o.pool.Put(&Node{
		InstanceID: "linode-1",
		State:      StateIdle,
		CreatedAt:  created,
	})

	got := o.PoolSnapshot()
	if len(got) != 1 {
		t.Fatalf("snapshot length: want 1 got %d", len(got))
	}
	w := got[0]
	if w.BillingModel != "hourly_round_up" {
		t.Errorf("BillingModel = %q, want hourly_round_up", w.BillingModel)
	}
	if want := created.Add(55 * time.Minute); !w.ReapEligibleAt.Equal(want) {
		t.Errorf("ReapEligibleAt = %s, want %s", w.ReapEligibleAt, want)
	}
	if want := created.Add(time.Hour); !w.PaidHourEndAt.Equal(want) {
		t.Errorf("PaidHourEndAt = %s, want %s", w.PaidHourEndAt, want)
	}
}
