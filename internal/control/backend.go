// Package control hosts the operator-facing control plane for a running
// fj-bellows daemon. It exposes a ConnectRPC service (Connect/JSON +
// gRPC + gRPC-Web on one handler) plus plain-HTTP /healthz and /metrics
// shims for ecosystem tooling.
package control

import (
	"context"
	"time"

	"github.com/hstern/fj-bellows/internal/control/events"
	"github.com/hstern/fj-bellows/internal/control/logbus"
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

	// CacheStatus returns the managed-cache snapshot for the provider that
	// owns one (today: Linode). Returns nil for providers without a cache
	// (docker) so the handler can answer Present=false. The Linode API
	// may be touched for live VM status — keep this off any hot path.
	CacheStatus(ctx context.Context) *CacheStatus

	// Kick drives a synchronous out-of-band reconcile and returns the per-tick
	// summary. Used by the Reconcile RPC.
	Kick(ctx context.Context) (ReconcileResult, error)

	// Subscribe returns a state-transition event stream + cancel func.
	// Used by the StreamEvents RPC. The channel closes when the caller
	// cancels OR when the bus drops the subscriber for slow consumption.
	Subscribe() (<-chan events.Event, func())

	// SubscribeLogs returns a structured-log stream + cancel func, scoped
	// to records that match filter (empty filter = every record). Used by
	// the StreamLogs RPC. The channel closes when the caller cancels OR
	// when the bus drops the subscriber for slow consumption.
	SubscribeLogs(filter logbus.Filter) (<-chan logbus.Record, func())

	// LogHistory returns up to n previously-buffered log records that
	// match filter, in chronological order (oldest first). Used by
	// StreamLogs to replay recent history before live streaming.
	LogHistory(n int, filter logbus.Filter) []logbus.Record

	// ForceReap immediately destroys the worker with the given instance
	// ID, bypassing billing policy. Audit-logged with the caller identity
	// threaded through the context. Returns an error if the instance
	// isn't in the pool or Destroy fails.
	ForceReap(ctx context.Context, instanceID string) error

	// ForceProvision spawns one extra worker, bypassing scale.max for
	// this single tick. Returns the new worker's instance ID on success;
	// async readiness errors surface later via the event stream.
	ForceProvision(ctx context.Context) (string, error)
}

// ReconcileResult is the per-tick summary returned by Kick. Counts are
// "intents started during the tick"; downstream goroutines may still be in
// flight when this surfaces.
type ReconcileResult struct {
	Provisioned int
	Dispatched  int
	Reaped      int
	Adopted     int
	Dropped     int
	Errors      []string
}

// CacheStatus is the shape control returns from GetCache. Mirrors the
// provider-side type one-for-one so the adapter stays trivial.
type CacheStatus struct {
	Present         bool
	AdoptedExisting bool
	LinodeID        int
	VPCIP           string
	BucketRegion    string
	BucketLabel     string
	VMState         string
}

// WorkerView is the per-node shape the control plane returns from ListWorkers.
// Mirrors orchestrator.Node plus the in-flight job handle and the billing-
// window snapshot (FJB-30) computed from the current TeardownPolicy.
type WorkerView struct {
	InstanceID string
	State      string
	IP         string
	CreatedAt  time.Time
	LastBusy   time.Time
	CurrentJob string

	// PaidHourEndAt is the next paid-hour boundary for the worker — when
	// hourly-round-up billing models close out the next paid hour. Zero
	// for per-second models.
	PaidHourEndAt time.Time
	// ReapEligibleAt is the earliest time the policy will tear this worker
	// down (LastBusy + IdleTimeout for per-second; the next :55 mark for
	// hourly).
	ReapEligibleAt time.Time
	// BillingModel is "per_second" or "hourly_round_up". Empty when the
	// policy is the zero value.
	BillingModel string
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
