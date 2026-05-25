package orchestrator

import (
	"context"
	"time"
)

// HealthStatus is the orchestrator's view of its own readiness. The control
// plane's Health endpoint consumes it; the threshold for Healthy is
// 3 * PollInterval since the last successful tick / probe.
type HealthStatus struct {
	Healthy            bool
	LastTickAt         time.Time
	LastProviderListAt time.Time
	LastForgejoPollAt  time.Time
}

// Health returns a snapshot of the freshness counters. The ctx is accepted to
// match a future interface where the answer might require an upstream probe;
// today it is unused.
func (o *Orchestrator) Health(_ context.Context) HealthStatus {
	o.mu.Lock()
	tick := o.lastTickAt
	prov := o.lastProviderListAt
	fj := o.lastForgejoPollAt
	o.mu.Unlock()

	threshold := 3 * o.cfg.PollInterval
	now := o.now()
	healthy := !tick.IsZero() &&
		now.Sub(tick) <= threshold &&
		!prov.IsZero() && now.Sub(prov) <= threshold &&
		!fj.IsZero() && now.Sub(fj) <= threshold

	return HealthStatus{
		Healthy:            healthy,
		LastTickAt:         tick,
		LastProviderListAt: prov,
		LastForgejoPollAt:  fj,
	}
}

func (o *Orchestrator) markTick() {
	o.mu.Lock()
	o.lastTickAt = o.now()
	o.mu.Unlock()
}

func (o *Orchestrator) markProviderList() {
	o.mu.Lock()
	o.lastProviderListAt = o.now()
	o.mu.Unlock()
}

func (o *Orchestrator) markForgejoPoll() {
	o.mu.Lock()
	o.lastForgejoPollAt = o.now()
	o.mu.Unlock()
}
