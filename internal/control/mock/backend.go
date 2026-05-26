// Package mock provides a hand-written fake Backend for the control package's
// tests. Func-field fakes match the convention used in internal/provider/mock
// and internal/orchestrator/mock.
package mock

import (
	"context"
	"sync"

	"github.com/hstern/fj-bellows/internal/control"
	"github.com/hstern/fj-bellows/internal/control/events"
	"github.com/hstern/fj-bellows/internal/control/logbus"
)

// Backend is a fake control.Backend. Unset func fields default to a
// zero-value response so a forgotten wire-up still produces valid (empty)
// data without panicking.
type Backend struct {
	mu                 sync.Mutex
	healthFn           func(ctx context.Context) control.HealthStatus
	poolSnapshotFn     func() []control.WorkerView
	cacheStatusFn      func(ctx context.Context) *control.CacheStatus
	kickFn             func(ctx context.Context) (control.ReconcileResult, error)
	subscribeFn        func() (<-chan events.Event, func())
	subscribeLogsFn    func(filter logbus.Filter) (<-chan logbus.Record, func())
	logHistoryFn       func(n int, filter logbus.Filter) []logbus.Record
	forceReapFn        func(ctx context.Context, instanceID string) error
	forceProvisionFn   func(ctx context.Context) (string, error)
	healthCall         int
	poolSnapshotCall   int
	cacheStatusCall    int
	kickCall           int
	subscribeCall      int
	subscribeLogsCall  int
	logHistoryCall     int
	forceReapCall      int
	forceProvisionCall int
}

// SetHealth installs the response for subsequent Health calls.
func (b *Backend) SetHealth(fn func(ctx context.Context) control.HealthStatus) {
	b.mu.Lock()
	b.healthFn = fn
	b.mu.Unlock()
}

// SetPoolSnapshot installs the response for subsequent PoolSnapshot calls.
func (b *Backend) SetPoolSnapshot(fn func() []control.WorkerView) {
	b.mu.Lock()
	b.poolSnapshotFn = fn
	b.mu.Unlock()
}

// SetCacheStatus installs the response for subsequent CacheStatus calls.
func (b *Backend) SetCacheStatus(fn func(ctx context.Context) *control.CacheStatus) {
	b.mu.Lock()
	b.cacheStatusFn = fn
	b.mu.Unlock()
}

// Health implements control.Backend.
func (b *Backend) Health(ctx context.Context) control.HealthStatus {
	b.mu.Lock()
	fn := b.healthFn
	b.healthCall++
	b.mu.Unlock()
	if fn == nil {
		return control.HealthStatus{}
	}
	return fn(ctx)
}

// PoolSnapshot implements control.Backend.
func (b *Backend) PoolSnapshot() []control.WorkerView {
	b.mu.Lock()
	fn := b.poolSnapshotFn
	b.poolSnapshotCall++
	b.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// CacheStatus implements control.Backend.
func (b *Backend) CacheStatus(ctx context.Context) *control.CacheStatus {
	b.mu.Lock()
	fn := b.cacheStatusFn
	b.cacheStatusCall++
	b.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx)
}

// CacheStatusCalls returns the number of times CacheStatus has been invoked.
func (b *Backend) CacheStatusCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cacheStatusCall
}

// SetKick installs the response for subsequent Kick calls.
func (b *Backend) SetKick(fn func(ctx context.Context) (control.ReconcileResult, error)) {
	b.mu.Lock()
	b.kickFn = fn
	b.mu.Unlock()
}

// Kick implements control.Backend.
func (b *Backend) Kick(ctx context.Context) (control.ReconcileResult, error) {
	b.mu.Lock()
	fn := b.kickFn
	b.kickCall++
	b.mu.Unlock()
	if fn == nil {
		return control.ReconcileResult{}, nil
	}
	return fn(ctx)
}

// KickCalls returns the number of times Kick has been invoked.
func (b *Backend) KickCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.kickCall
}

// SetSubscribe installs the response for subsequent Subscribe calls.
func (b *Backend) SetSubscribe(fn func() (<-chan events.Event, func())) {
	b.mu.Lock()
	b.subscribeFn = fn
	b.mu.Unlock()
}

// Subscribe implements control.Backend.
func (b *Backend) Subscribe() (<-chan events.Event, func()) {
	b.mu.Lock()
	fn := b.subscribeFn
	b.subscribeCall++
	b.mu.Unlock()
	if fn == nil {
		ch := make(chan events.Event)
		close(ch)
		return ch, func() {}
	}
	return fn()
}

// SubscribeCalls returns the number of times Subscribe has been invoked.
func (b *Backend) SubscribeCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subscribeCall
}

// SetSubscribeLogs installs the response for subsequent SubscribeLogs calls.
func (b *Backend) SetSubscribeLogs(fn func(filter logbus.Filter) (<-chan logbus.Record, func())) {
	b.mu.Lock()
	b.subscribeLogsFn = fn
	b.mu.Unlock()
}

// SubscribeLogs implements control.Backend.
func (b *Backend) SubscribeLogs(filter logbus.Filter) (<-chan logbus.Record, func()) {
	b.mu.Lock()
	fn := b.subscribeLogsFn
	b.subscribeLogsCall++
	b.mu.Unlock()
	if fn == nil {
		ch := make(chan logbus.Record)
		close(ch)
		return ch, func() {}
	}
	return fn(filter)
}

// SubscribeLogsCalls returns the number of times SubscribeLogs has been invoked.
func (b *Backend) SubscribeLogsCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subscribeLogsCall
}

// SetLogHistory installs the response for subsequent LogHistory calls.
func (b *Backend) SetLogHistory(fn func(n int, filter logbus.Filter) []logbus.Record) {
	b.mu.Lock()
	b.logHistoryFn = fn
	b.mu.Unlock()
}

// LogHistory implements control.Backend.
func (b *Backend) LogHistory(n int, filter logbus.Filter) []logbus.Record {
	b.mu.Lock()
	fn := b.logHistoryFn
	b.logHistoryCall++
	b.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(n, filter)
}

// LogHistoryCalls returns the number of times LogHistory has been invoked.
func (b *Backend) LogHistoryCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.logHistoryCall
}

// SetForceReap installs the response for subsequent ForceReap calls.
func (b *Backend) SetForceReap(fn func(ctx context.Context, instanceID string) error) {
	b.mu.Lock()
	b.forceReapFn = fn
	b.mu.Unlock()
}

// ForceReap implements control.Backend.
func (b *Backend) ForceReap(ctx context.Context, instanceID string) error {
	b.mu.Lock()
	fn := b.forceReapFn
	b.forceReapCall++
	b.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx, instanceID)
}

// ForceReapCalls returns the number of times ForceReap has been invoked.
func (b *Backend) ForceReapCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.forceReapCall
}

// SetForceProvision installs the response for subsequent ForceProvision calls.
func (b *Backend) SetForceProvision(fn func(ctx context.Context) (string, error)) {
	b.mu.Lock()
	b.forceProvisionFn = fn
	b.mu.Unlock()
}

// ForceProvision implements control.Backend.
func (b *Backend) ForceProvision(ctx context.Context) (string, error) {
	b.mu.Lock()
	fn := b.forceProvisionFn
	b.forceProvisionCall++
	b.mu.Unlock()
	if fn == nil {
		return "", nil
	}
	return fn(ctx)
}

// ForceProvisionCalls returns the number of times ForceProvision has been invoked.
func (b *Backend) ForceProvisionCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.forceProvisionCall
}

// HealthCalls returns the number of times Health has been invoked.
func (b *Backend) HealthCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.healthCall
}

// PoolSnapshotCalls returns the number of times PoolSnapshot has been invoked.
func (b *Backend) PoolSnapshotCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.poolSnapshotCall
}
