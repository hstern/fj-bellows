package cachegateway

import (
	"strings"
	"testing"
)

func stockWGInputs() WGInputs {
	return WGInputs{
		CachePrivateKey:        "kCachePrivKeyBase64Placeholder42424242424242424=",
		CacheWGAddr:            "10.99.0.2/32",
		ListenPort:             51820,
		OrchestratorPublicKey:  "kOrchPubKeyBase64Placeholder42424242424242424=",
		OrchestratorAllowedIPs: []string{"10.99.0.1/32"},
		WorkerVPCSubnet:        "10.0.0.0/24",
	}
}

func TestRenderWGQuick_HappyPath(t *testing.T) {
	got, err := RenderWGQuick(stockWGInputs())
	if err != nil {
		t.Fatalf("RenderWGQuick: %v", err)
	}
	wantSubstrings := []string{
		"[Interface]",
		"Address = 10.99.0.2/32",
		"ListenPort = 51820",
		"PrivateKey = kCachePrivKeyBase64Placeholder42424242424242424=",
		"[Peer]",
		"PublicKey = kOrchPubKeyBase64Placeholder42424242424242424=",
		"AllowedIPs = 10.99.0.1/32",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in wg0.conf:\n%s", sub, got)
		}
	}
}

// Endpoint and PersistentKeepalive belong only on the orchestrator side
// (the NAT-traversing initiator) — the cache config must NOT emit them.
func TestRenderWGQuick_OmitsEndpointAndKeepalive(t *testing.T) {
	got, err := RenderWGQuick(stockWGInputs())
	if err != nil {
		t.Fatalf("RenderWGQuick: %v", err)
	}
	notWanted := []string{
		"Endpoint =",
		"PersistentKeepalive =",
	}
	for _, sub := range notWanted {
		if strings.Contains(got, sub) {
			t.Errorf("cache wg0.conf must NOT emit %q (only initiator side does):\n%s", sub, got)
		}
	}
}

// Multiple AllowedIPs join with ", " — wg-quick's expected list shape.
func TestRenderWGQuick_MultipleAllowedIPs(t *testing.T) {
	in := stockWGInputs()
	in.OrchestratorAllowedIPs = []string{"10.99.0.1/32", "192.168.0.0/24"}
	got, err := RenderWGQuick(in)
	if err != nil {
		t.Fatalf("RenderWGQuick: %v", err)
	}
	if !strings.Contains(got, "AllowedIPs = 10.99.0.1/32, 192.168.0.0/24") {
		t.Errorf("multiple AllowedIPs not joined with comma+space:\n%s", got)
	}
}

func TestWGInputs_Validation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*WGInputs)
		wantSub string
	}{
		{"no private key", func(i *WGInputs) { i.CachePrivateKey = "" }, "CachePrivateKey is required"},
		{"no cache WG addr", func(i *WGInputs) { i.CacheWGAddr = "" }, "CacheWGAddr is required"},
		{"port too low", func(i *WGInputs) { i.ListenPort = 0 }, "ListenPort 0 out of range"},
		{"port too high", func(i *WGInputs) { i.ListenPort = 99999 }, "ListenPort 99999 out of range"},
		{"no peer public key", func(i *WGInputs) { i.OrchestratorPublicKey = "" }, "OrchestratorPublicKey is required"},
		{"no allowed_ips", func(i *WGInputs) { i.OrchestratorAllowedIPs = nil }, "OrchestratorAllowedIPs must list at least one CIDR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := stockWGInputs()
			tc.mutate(&in)
			_, err := RenderWGQuick(in)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err, tc.wantSub)
			}
		})
	}
}
