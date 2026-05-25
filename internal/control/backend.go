// Package control hosts the operator-facing control plane for a running
// fj-bellows daemon. It exposes a ConnectRPC service (Connect/JSON +
// gRPC + gRPC-Web on one handler) plus plain-HTTP /healthz and /metrics
// shims for ecosystem tooling.
package control

import (
	"context"
	"time"
)

// Backend is the slice of the orchestrator that the control plane needs.
// *orchestrator.Orchestrator implements it; tests supply a fake from the
// sibling mock/ package.
type Backend interface {
	// Health returns a readiness snapshot. Implementations should be cheap;
	// the handler may call this many times per second under k8s liveness.
	Health(ctx context.Context) HealthStatus

	// PoolSnapshot returns the orchestrator's current view of the worker pool.
	// Used by ListWorkers; cheap (one mutex acquisition + slice copy).
	PoolSnapshot() []WorkerView
}

// WorkerView is the per-node shape the control plane returns from ListWorkers.
// Mirrors orchestrator.Node plus the in-flight job handle.
type WorkerView struct {
	InstanceID string
	State      string
	IP         string
	CreatedAt  time.Time
	LastBusy   time.Time
	CurrentJob string
}

// HealthStatus is the orchestrator's view of its own readiness.
type HealthStatus struct {
	// Healthy is true when every signal below is within the freshness
	// threshold (3 * poll_interval). A daemon that just started reports
	// healthy only after its first completed reconcile.
	Healthy bool

	// LastTickAt is when the reconcile loop most recently completed.
	LastTickAt time.Time

	// LastProviderListAt is when prov.List most recently succeeded.
	LastProviderListAt time.Time

	// LastForgejoPollAt is when WaitingJobs or ListRunners most recently
	// succeeded; whichever was later.
	LastForgejoPollAt time.Time
}
