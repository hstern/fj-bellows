// Package mock provides hand-written mocks of the orchestrator's dependency
// interfaces (JobSource and Dispatcher) for unit tests. Methods delegate to
// function fields and record calls for assertions.
package mock

import (
	"context"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/hstern/fj-bellows/internal/forgejo"
)

// JobSource mocks orchestrator.JobSource.
type JobSource struct {
	WaitingJobsFn       func(ctx context.Context) ([]forgejo.WaitingJob, error)
	RegisterEphemeralFn func(ctx context.Context, name string, labels []string) (forgejo.Registration, error)
	ListRunnersFn       func(ctx context.Context) ([]forgejo.Runner, error)
	DeleteRunnerFn      func(ctx context.Context, id int64) error

	mu            sync.Mutex
	RegisterCalls []string // names passed to RegisterEphemeral
	DeleteCalls   []int64  // ids passed to DeleteRunner
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

// ListRunners delegates to ListRunnersFn if set.
func (m *JobSource) ListRunners(ctx context.Context) ([]forgejo.Runner, error) {
	if m.ListRunnersFn != nil {
		return m.ListRunnersFn(ctx)
	}
	return nil, nil
}

// DeleteRunner records the id and delegates to DeleteRunnerFn if set.
func (m *JobSource) DeleteRunner(ctx context.Context, id int64) error {
	m.mu.Lock()
	m.DeleteCalls = append(m.DeleteCalls, id)
	m.mu.Unlock()
	if m.DeleteRunnerFn != nil {
		return m.DeleteRunnerFn(ctx, id)
	}
	return nil
}

// DeleteCount returns how many times DeleteRunner was called.
func (m *JobSource) DeleteCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.DeleteCalls)
}

// Dispatcher mocks orchestrator.Dispatcher.
type Dispatcher struct {
	WaitReadyFn func(ctx context.Context, id, addr string) error
	RunJobFn    func(ctx context.Context, id, addr string, reg forgejo.Registration, job forgejo.WaitingJob) error

	mu       sync.Mutex
	RunCalls []forgejo.WaitingJob
}

// WaitReady delegates to WaitReadyFn if set.
func (m *Dispatcher) WaitReady(ctx context.Context, id, addr string) error {
	if m.WaitReadyFn != nil {
		return m.WaitReadyFn(ctx, id, addr)
	}
	return nil
}

// RunJob records the job and delegates to RunJobFn if set.
func (m *Dispatcher) RunJob(ctx context.Context, id, addr string, reg forgejo.Registration, job forgejo.WaitingJob) error {
	m.mu.Lock()
	m.RunCalls = append(m.RunCalls, job)
	m.mu.Unlock()
	if m.RunJobFn != nil {
		return m.RunJobFn(ctx, id, addr, reg, job)
	}
	return nil
}

// RunCount returns how many times RunJob was called.
func (m *Dispatcher) RunCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.RunCalls)
}

// PinningDispatcher is a Dispatcher that also implements
// orchestrator.HostKeyPinner, recording the host keys it is asked to pin. It is
// a distinct type so the plain Dispatcher does not accidentally satisfy
// HostKeyPinner and change provisioning behavior in existing tests.
type PinningDispatcher struct {
	Dispatcher

	pmu      sync.Mutex
	pinnedIP map[string]ssh.PublicKey
}

// PinHostKey records key as the pinned host key for ip.
func (m *PinningDispatcher) PinHostKey(ip string, key ssh.PublicKey) {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if m.pinnedIP == nil {
		m.pinnedIP = make(map[string]ssh.PublicKey)
	}
	m.pinnedIP[ip] = key
}

// PinnedKey returns the host key pinned for ip and whether one was recorded.
func (m *PinningDispatcher) PinnedKey(ip string) (ssh.PublicKey, bool) {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	k, ok := m.pinnedIP[ip]
	return k, ok
}

// PinCount returns how many distinct IPs have had a host key pinned.
func (m *PinningDispatcher) PinCount() int {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	return len(m.pinnedIP)
}
