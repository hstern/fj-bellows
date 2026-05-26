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
	// Paused reflects the reconciler's auto-tick suppression flag (FJB-27).
	// Operator-visible signal: when true, the freshness counters will go
	// stale because no real reconcile is firing, so Healthy will flip to
	// false on its own. The flag distinguishes "intentionally quiesced"
	// from "stuck upstream".
	Paused bool
}

// WorkerView is the per-node shape returned by PoolSnapshot. Stable,
// wire-friendly mirror of orchestrator.Node so the control plane can
// translate to protobuf without coupling the orchestrator to generated code.
type WorkerView struct {
	InstanceID string
	State      string
	IP         string
	CreatedAt  time.Time
	LastBusy   time.Time
	CurrentJob string
}

// PoolSnapshot returns a copy of every node currently in the pool.
// Equivalent of Pool.Snapshot but stringified for the wire (NodeState → string).
func (o *Orchestrator) PoolSnapshot() []WorkerView {
	nodes := o.pool.Snapshot()
	out := make([]WorkerView, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, WorkerView{
			InstanceID: n.InstanceID,
			State:      string(n.State),
			IP:         n.IP,
			CreatedAt:  n.CreatedAt,
			LastBusy:   n.LastBusy,
			CurrentJob: n.CurrentJob,
		})
	}
	return out
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
		Paused:             o.paused.Load(),
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
