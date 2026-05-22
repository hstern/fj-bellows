// Package mock provides hand-written mocks of the orchestrator's dependency
// interfaces (JobSource and Dispatcher) for unit tests. Methods delegate to
// function fields and record calls for assertions.
package mock

import (
	"context"
	"sync"

	"github.com/hstern/fj-bellows/internal/forgejo"
)

// JobSource mocks orchestrator.JobSource.
type JobSource struct {
	WaitingJobsFn       func(ctx context.Context) ([]forgejo.WaitingJob, error)
	RegisterEphemeralFn func(ctx context.Context, name string, labels []string) (forgejo.Registration, error)

	mu            sync.Mutex
	RegisterCalls []string // names passed to RegisterEphemeral
}

// WaitingJobs delegates to WaitingJobsFn if set.
func (m *JobSource) WaitingJobs(ctx context.Context) ([]forgejo.WaitingJob, error) {
	if m.WaitingJobsFn != nil {
		return m.WaitingJobsFn(ctx)
	}
	return nil, nil
}

// RegisterEphemeral records the name and delegates to RegisterEphemeralFn if set.
func (m *JobSource) RegisterEphemeral(ctx context.Context, name string, labels []string) (forgejo.Registration, error) {
	m.mu.Lock()
	m.RegisterCalls = append(m.RegisterCalls, name)
	m.mu.Unlock()
	if m.RegisterEphemeralFn != nil {
		return m.RegisterEphemeralFn(ctx, name, labels)
	}
	return forgejo.Registration{UUID: "uuid", Token: "token"}, nil
}

// RegisterCount returns how many times RegisterEphemeral was called.
func (m *JobSource) RegisterCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.RegisterCalls)
}

// Dispatcher mocks orchestrator.Dispatcher.
type Dispatcher struct {
	WaitReadyFn func(ctx context.Context, ip string) error
	RunJobFn    func(ctx context.Context, ip string, reg forgejo.Registration, job forgejo.WaitingJob) error

	mu       sync.Mutex
	RunCalls []forgejo.WaitingJob
}

// WaitReady delegates to WaitReadyFn if set.
func (m *Dispatcher) WaitReady(ctx context.Context, ip string) error {
	if m.WaitReadyFn != nil {
		return m.WaitReadyFn(ctx, ip)
	}
	return nil
}

// RunJob records the job and delegates to RunJobFn if set.
func (m *Dispatcher) RunJob(ctx context.Context, ip string, reg forgejo.Registration, job forgejo.WaitingJob) error {
	m.mu.Lock()
	m.RunCalls = append(m.RunCalls, job)
	m.mu.Unlock()
	if m.RunJobFn != nil {
		return m.RunJobFn(ctx, ip, reg, job)
	}
	return nil
}

// RunCount returns how many times RunJob was called.
func (m *Dispatcher) RunCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.RunCalls)
}
