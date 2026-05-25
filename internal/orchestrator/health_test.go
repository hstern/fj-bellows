package orchestrator

import (
	"testing"
	"time"
)

func TestHealth_ZeroTimestamps_Unhealthy(t *testing.T) {
	o := newTestOrch(t, 5*time.Second)
	s := o.Health(t.Context())
	if s.Healthy {
		t.Fatalf("zero-timestamp orchestrator must be unhealthy")
	}
	if !s.LastTickAt.IsZero() {
		t.Fatalf("LastTickAt should be zero before first tick")
	}
}

func TestHealth_RecentSignals_Healthy(t *testing.T) {
	o := newTestOrch(t, 5*time.Second)
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	o.now = func() time.Time { return now }
	o.markTick()
	o.markProviderList()
	o.markForgejoPoll()

	s := o.Health(t.Context())
	if !s.Healthy {
		t.Fatalf("all signals fresh-as-of-now should be healthy, got %+v", s)
	}
}

func TestHealth_StaleTick_Unhealthy(t *testing.T) {
	o := newTestOrch(t, 5*time.Second)
	// Tick happened 30s ago, threshold is 15s → unhealthy.
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	o.now = func() time.Time { return now.Add(-30 * time.Second) }
	o.markTick()
	o.markProviderList()
	o.markForgejoPoll()

	o.now = func() time.Time { return now }
	if s := o.Health(t.Context()); s.Healthy {
		t.Fatalf("stale signals should be unhealthy")
	}
}

// newTestOrch builds the smallest Orchestrator that exercises Health() —
// nil dependencies are fine because Health doesn't touch them.
func newTestOrch(t *testing.T, poll time.Duration) *Orchestrator {
	t.Helper()
	return &Orchestrator{
		cfg: Config{PollInterval: poll},
		now: time.Now,
	}
}
