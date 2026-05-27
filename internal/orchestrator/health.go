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
//
// The billing-window fields (PaidHourEndAt, ReapEligibleAt, BillingModel)
// are populated from the orchestrator's TeardownPolicy via Timing(); see
// FJB-30.
type WorkerView struct {
	InstanceID string
	State      string
	// IP is the public IPv4 (legacy dial address under ssh transport).
	IP string
	// VPCIP is the VPC-side IPv4 (dial address under cache-gateway
	// transport, FJB-54). Empty when no VPC is configured.
	VPCIP      string
	CreatedAt  time.Time
	LastBusy   time.Time
	CurrentJob string

	// PaidHourEndAt is the next paid-hour boundary for the worker — when
	// hourly-round-up billing models close out the next paid hour. Zero
	// for per-second models.
	PaidHourEndAt time.Time
	// ReapEligibleAt is when the worker first becomes eligible for
	// teardown under the current policy: LastBusy + IdleTimeout for
	// per-second, the next :55 mark for hourly.
	ReapEligibleAt time.Time
	// BillingModel is the policy's billing model string:
	// "per_second" | "hourly_round_up". Empty for the zero policy.
	BillingModel string
}

// PoolSnapshot returns a copy of every node currently in the pool.
// Equivalent of Pool.Snapshot but stringified for the wire (NodeState → string).
// Each view also carries the per-worker billing-window timing computed
// from the current TeardownPolicy.
func (o *Orchestrator) PoolSnapshot() []WorkerView {
	nodes := o.pool.Snapshot()
	now := o.now()
	out := make([]WorkerView, 0, len(nodes))
	for _, n := range nodes {
		t := o.cfg.Teardown.Timing(n, now)
		out = append(out, WorkerView{
			InstanceID:     n.InstanceID,
			State:          string(n.State),
			IP:             n.IP,
			VPCIP:          n.VPCIP,
			CreatedAt:      n.CreatedAt,
			LastBusy:       n.LastBusy,
			CurrentJob:     n.CurrentJob,
			PaidHourEndAt:  t.PaidHourEndAt,
			ReapEligibleAt: t.ReapEligibleAt,
			BillingModel:   t.BillingModel,
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

// TransportModeCacheGateway is the value of Config.TransportMode that
// selects the FJB-54 cache-as-gateway dispatch path: dial workers by
// VPC IP through the IPsec tunnel terminated on the cache nanode.
//
// Mirrors config.TransportCacheGateway as a literal so this package
// doesn't take a dependency on internal/config.
const TransportModeCacheGateway = "cache-gateway"

// addrFor returns the address the dispatcher should dial for the given
// node, branching on the active transport mode. Empty / "ssh" (default)
// returns the public IPv4 (legacy path); "cache-gateway" (FJB-54)
// returns the VPC IP, routed via the IPsec tunnel.
//
// Defined on Orchestrator (not Node) because the choice is a
// composition-root concern, not a per-node property.
func (o *Orchestrator) addrFor(n *Node) string {
	if o.cfg.TransportMode == TransportModeCacheGateway {
		return n.VPCIP
	}
	return n.IP
}

// addrForInstance is the just-provisioned counterpart of addrFor when
// the caller has a provider.Instance in hand but hasn't yet retrieved
// the Node from the pool. Same selection rule.
func (o *Orchestrator) addrForInstance(ip4, vpcIP string) string {
	if o.cfg.TransportMode == TransportModeCacheGateway {
		return vpcIP
	}
	return ip4
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
