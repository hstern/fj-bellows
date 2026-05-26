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
	pauseFn            func(ctx context.Context)
	resumeFn           func(ctx context.Context)
	getConfigFn        func(ctx context.Context) (string, string)
	reloadConfigFn     func(ctx context.Context) ([]string, error)
	execOnWorkerFn     func(ctx context.Context, instanceID, command string) ([]byte, []byte, int32, int64, int64, error)
	providerInfoFn     func(ctx context.Context) (string, map[string]string)
	healthCall         int
	poolSnapshotCall   int
	cacheStatusCall    int
	kickCall           int
	subscribeCall      int
	subscribeLogsCall  int
	logHistoryCall     int
	forceReapCall      int
	forceProvisionCall int
	pauseCall          int
	resumeCall         int
	getConfigCall      int
	reloadConfigCall   int
	execOnWorkerCall   int
	providerInfoCall   int
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

// SetPause installs the response for subsequent Pause calls.
func (b *Backend) SetPause(fn func(ctx context.Context)) {
	b.mu.Lock()
	b.pauseFn = fn
	b.mu.Unlock()
}

// Pause implements control.Backend.
func (b *Backend) Pause(ctx context.Context) {
	b.mu.Lock()
	fn := b.pauseFn
	b.pauseCall++
	b.mu.Unlock()
	if fn == nil {
		return
	}
	fn(ctx)
}

// PauseCalls returns the number of times Pause has been invoked.
func (b *Backend) PauseCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pauseCall
}

// SetResume installs the response for subsequent Resume calls.
func (b *Backend) SetResume(fn func(ctx context.Context)) {
	b.mu.Lock()
	b.resumeFn = fn
	b.mu.Unlock()
}

// Resume implements control.Backend.
func (b *Backend) Resume(ctx context.Context) {
	b.mu.Lock()
	fn := b.resumeFn
	b.resumeCall++
	b.mu.Unlock()
	if fn == nil {
		return
	}
	fn(ctx)
}

// ResumeCalls returns the number of times Resume has been invoked.
func (b *Backend) ResumeCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.resumeCall
}

// SetGetConfig installs the response for subsequent GetConfig calls.
func (b *Backend) SetGetConfig(fn func(ctx context.Context) (string, string)) {
	b.mu.Lock()
	b.getConfigFn = fn
	b.mu.Unlock()
}

// GetConfig implements control.Backend.
func (b *Backend) GetConfig(ctx context.Context) (string, string) {
	b.mu.Lock()
	fn := b.getConfigFn
	b.getConfigCall++
	b.mu.Unlock()
	if fn == nil {
		return "", ""
	}
	return fn(ctx)
}

// GetConfigCalls returns the number of times GetConfig has been invoked.
func (b *Backend) GetConfigCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.getConfigCall
}

// SetReloadConfig installs the response for subsequent ReloadConfig calls.
func (b *Backend) SetReloadConfig(fn func(ctx context.Context) ([]string, error)) {
	b.mu.Lock()
	b.reloadConfigFn = fn
	b.mu.Unlock()
}

// ReloadConfig implements control.Backend.
func (b *Backend) ReloadConfig(ctx context.Context) ([]string, error) {
	b.mu.Lock()
	fn := b.reloadConfigFn
	b.reloadConfigCall++
	b.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(ctx)
}

// ReloadConfigCalls returns the number of times ReloadConfig has been invoked.
func (b *Backend) ReloadConfigCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reloadConfigCall
}

// SetExecOnWorker installs the response for subsequent ExecOnWorker calls.
func (b *Backend) SetExecOnWorker(fn func(ctx context.Context, instanceID, command string) ([]byte, []byte, int32, int64, int64, error)) {
	b.mu.Lock()
	b.execOnWorkerFn = fn
	b.mu.Unlock()
}

// ExecOnWorker implements control.Backend.
func (b *Backend) ExecOnWorker(ctx context.Context, instanceID, command string) ([]byte, []byte, int32, int64, int64, error) {
	b.mu.Lock()
	fn := b.execOnWorkerFn
	b.execOnWorkerCall++
	b.mu.Unlock()
	if fn == nil {
		return nil, nil, 0, 0, 0, nil
	}
	return fn(ctx, instanceID, command)
}

// ExecOnWorkerCalls returns the number of times ExecOnWorker has been invoked.
func (b *Backend) ExecOnWorkerCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.execOnWorkerCall
}

// SetProviderInfo installs the response for subsequent ProviderInfo calls.
func (b *Backend) SetProviderInfo(fn func(ctx context.Context) (string, map[string]string)) {
	b.mu.Lock()
	b.providerInfoFn = fn
	b.mu.Unlock()
}

// ProviderInfo implements control.Backend.
func (b *Backend) ProviderInfo(ctx context.Context) (string, map[string]string) {
	b.mu.Lock()
	fn := b.providerInfoFn
	b.providerInfoCall++
	b.mu.Unlock()
	if fn == nil {
		return "", map[string]string{}
	}
	return fn(ctx)
}

// ProviderInfoCalls returns the number of times ProviderInfo has been invoked.
func (b *Backend) ProviderInfoCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.providerInfoCall
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
