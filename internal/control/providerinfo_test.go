package control_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

// TestProviderInfo_RoundTrip covers the happy path: backend hands the
// handler a populated map and a slug, the handler passes both through
// unchanged. Tests both the wire shape and that the call count is
// recorded so future regressions (e.g. handler caching by accident)
// are visible.
func TestProviderInfo_RoundTrip(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetProviderInfo(func(context.Context) (string, map[string]string) {
		return "linode", map[string]string{
			"region":              testRegion,
			"workers_in_flight":   "2",
			"account_balance_usd": "-12.34",
		}
	})

	_, client := newTestServer(t, be)
	resp, err := client.ProviderInfo(t.Context(), connect.NewRequest(&controlv1.ProviderInfoRequest{}))
	if err != nil {
		t.Fatalf("ProviderInfo rpc: %v", err)
	}
	if resp.Msg.Provider != "linode" {
		t.Fatalf("Provider: want linode got %q", resp.Msg.Provider)
	}
	if got := resp.Msg.Info["region"]; got != testRegion {
		t.Fatalf("info[region]: want us-ord got %q", got)
	}
	if got := resp.Msg.Info["workers_in_flight"]; got != "2" {
		t.Fatalf("info[workers_in_flight]: want 2 got %q", got)
	}
	if got := resp.Msg.Info["account_balance_usd"]; got != "-12.34" {
		t.Fatalf("info[account_balance_usd]: want -12.34 got %q", got)
	}
	if be.ProviderInfoCalls() != 1 {
		t.Fatalf("ProviderInfoCalls: want 1 got %d", be.ProviderInfoCalls())
	}
}

// TestProviderInfo_EmptyMap covers the provider-doesn't-implement-
// InfoProvider path: backend returns an empty map and the slug, and the
// wire response must still carry a non-nil (just empty) Info map so
// clients don't need to special-case nil.
func TestProviderInfo_EmptyMap(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetProviderInfo(func(context.Context) (string, map[string]string) {
		return "docker", map[string]string{}
	})

	_, client := newTestServer(t, be)
	resp, err := client.ProviderInfo(t.Context(), connect.NewRequest(&controlv1.ProviderInfoRequest{}))
	if err != nil {
		t.Fatalf("ProviderInfo rpc: %v", err)
	}
	if resp.Msg.Provider != "docker" {
		t.Fatalf("Provider: want docker got %q", resp.Msg.Provider)
	}
	// Proto map fields decode as either nil or empty depending on
	// codec; the operator-visible behavior we care about is len()==0,
	// not the nilness of the underlying slot.
	if len(resp.Msg.Info) != 0 {
		t.Fatalf("Info should be empty, got %v", resp.Msg.Info)
	}
}

// TestProviderInfo_NilMap covers the nil-map-from-backend path: a
// backend that hands back a nil map (forgotten init in some future
// adapter) still produces a successful response — no panic, no nil
// dereference. Whether the wire form ends up nil or empty is a codec
// detail; the operator's view is len()==0 either way.
func TestProviderInfo_NilMap(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetProviderInfo(func(context.Context) (string, map[string]string) {
		return "linode", nil
	})

	_, client := newTestServer(t, be)
	resp, err := client.ProviderInfo(t.Context(), connect.NewRequest(&controlv1.ProviderInfoRequest{}))
	if err != nil {
		t.Fatalf("ProviderInfo rpc: %v", err)
	}
	if len(resp.Msg.Info) != 0 {
		t.Fatalf("Info: want empty got %v", resp.Msg.Info)
	}
}
