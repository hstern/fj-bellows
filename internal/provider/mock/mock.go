// Package mock provides a hand-written, configurable mock of
// provider.Provider for unit tests. Each method delegates to a function field;
// unset fields return zero values so tests set only what they exercise.
package mock

import (
	"context"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

// Provider is a configurable provider.Provider. It records calls for
// assertions and is safe for concurrent use.
type Provider struct {
	ConfigureFn    func(node yaml.Node) error
	ProvisionFn    func(ctx context.Context, spec provider.Spec) (provider.Instance, error)
	DestroyFn      func(ctx context.Context, id string) error
	ListFn         func(ctx context.Context, tag string) ([]provider.Instance, error)
	BillingModelFn func() provider.BillingModel

	mu             sync.Mutex
	ProvisionCalls []provider.Spec
	DestroyCalls   []string
	ListCalls      int
}

var _ provider.Provider = (*Provider)(nil)

// Configure delegates to ConfigureFn if set.
func (m *Provider) Configure(node yaml.Node) error {
	if m.ConfigureFn != nil {
		return m.ConfigureFn(node)
	}
	return nil
}

// Provision records the spec and delegates to ProvisionFn if set.
func (m *Provider) Provision(ctx context.Context, spec provider.Spec) (provider.Instance, error) {
	m.mu.Lock()
	m.ProvisionCalls = append(m.ProvisionCalls, spec)
	m.mu.Unlock()
	if m.ProvisionFn != nil {
		return m.ProvisionFn(ctx, spec)
	}
	return provider.Instance{}, nil
}

// Destroy records the id and delegates to DestroyFn if set.
func (m *Provider) Destroy(ctx context.Context, id string) error {
	m.mu.Lock()
	m.DestroyCalls = append(m.DestroyCalls, id)
	m.mu.Unlock()
	if m.DestroyFn != nil {
		return m.DestroyFn(ctx, id)
	}
	return nil
}

// List counts the call and delegates to ListFn if set.
func (m *Provider) List(ctx context.Context, tag string) ([]provider.Instance, error) {
	m.mu.Lock()
	m.ListCalls++
	m.mu.Unlock()
	if m.ListFn != nil {
		return m.ListFn(ctx, tag)
	}
	return nil, nil
}

// BillingModel delegates to BillingModelFn if set, else returns BillingPerSecond.
func (m *Provider) BillingModel() provider.BillingModel {
	if m.BillingModelFn != nil {
		return m.BillingModelFn()
	}
	return provider.BillingPerSecond
}

// ProvisionCount returns how many times Provision was called.
func (m *Provider) ProvisionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ProvisionCalls)
}

// DestroyCount returns how many times Destroy was called.
func (m *Provider) DestroyCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.DestroyCalls)
}
