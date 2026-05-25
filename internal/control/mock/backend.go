// Package mock provides a hand-written fake Backend for the control package's
// tests. Func-field fakes match the convention used in internal/provider/mock
// and internal/orchestrator/mock.
package mock

import (
	"context"
	"sync"

	"github.com/hstern/fj-bellows/internal/control"
)

// Backend is a fake control.Backend. Unset func fields default to a
// zero-value response so a forgotten wire-up still produces valid (empty)
// data without panicking.
type Backend struct {
	mu               sync.Mutex
	healthFn         func(ctx context.Context) control.HealthStatus
	poolSnapshotFn   func() []control.WorkerView
	cacheStatusFn    func(ctx context.Context) *control.CacheStatus
	healthCall       int
	poolSnapshotCall int
	cacheStatusCall  int
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
