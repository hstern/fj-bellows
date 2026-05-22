package provider_test

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

type stub struct{}

func (stub) Configure(yaml.Node) error { return nil }
func (stub) Provision(context.Context, provider.Spec) (provider.Instance, error) {
	return provider.Instance{}, nil
}
func (stub) Destroy(context.Context, string) error                     { return nil }
func (stub) List(context.Context, string) ([]provider.Instance, error) { return nil, nil }
func (stub) BillingModel() provider.BillingModel                       { return provider.BillingPerSecond }

func TestRegisterAndNew(t *testing.T) {
	provider.Register("stub-test", func() provider.Provider { return stub{} })
	p, err := provider.New("stub-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(stub); !ok {
		t.Errorf("New returned %T", p)
	}
}

func TestNewUnknown(t *testing.T) {
	if _, err := provider.New("does-not-exist"); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestBillingModelString(t *testing.T) {
	cases := map[provider.BillingModel]string{
		provider.BillingPerSecond:     "per-second",
		provider.BillingHourlyRoundUp: "hourly-round-up",
		provider.BillingModel(99):     "unknown",
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", m, got, want)
		}
	}
}
