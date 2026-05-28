package wgboot

import (
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/hstern/fj-bellows/internal/config"
	"github.com/hstern/fj-bellows/internal/transport/wg/acl"
)

func TestBoot_RejectsNonCacheGatewayMode(t *testing.T) {
	cfg := Config{
		Transport: config.Transport{Mode: config.TransportSSH},
	}
	_, err := Boot(t.Context(), cfg)
	if err == nil {
		t.Fatal("Boot(ssh-mode) should error; got nil")
	}
	if !strings.Contains(err.Error(), "transport.mode") {
		t.Errorf("err = %v, want substring 'transport.mode'", err)
	}
}

func TestBoot_RejectsMissingWGBlock(t *testing.T) {
	cfg := Config{
		Transport: config.Transport{Mode: config.TransportCacheGateway},
	}
	_, err := Boot(t.Context(), cfg)
	if err == nil {
		t.Fatal("Boot(no wg) should error; got nil")
	}
	if !strings.Contains(err.Error(), "transport.wg") {
		t.Errorf("err = %v, want substring 'transport.wg'", err)
	}
}

func TestBoot_RejectsMissingForgejoURL(t *testing.T) {
	cfg := Config{
		Transport: config.Transport{
			Mode: config.TransportCacheGateway,
			WG:   &config.WG{},
		},
	}
	_, err := Boot(t.Context(), cfg)
	if err == nil {
		t.Fatal("Boot(no forgejo url) should error; got nil")
	}
	if !strings.Contains(err.Error(), "ForgejoURL") {
		t.Errorf("err = %v, want substring 'ForgejoURL'", err)
	}
}

func TestBoot_RejectsMissingCacheVPCIP(t *testing.T) {
	cfg := Config{
		Transport: config.Transport{
			Mode: config.TransportCacheGateway,
			WG:   &config.WG{},
		},
		ForgejoURL: "https://git.example.com",
	}
	_, err := Boot(t.Context(), cfg)
	if err == nil {
		t.Fatal("Boot(no cache VPC IP) should error; got nil")
	}
	if !strings.Contains(err.Error(), "CacheVPCIP") {
		t.Errorf("err = %v, want substring 'CacheVPCIP'", err)
	}
}

func TestRegistryAdapter_ExcludesCacheHost(t *testing.T) {
	entries, err := acl.Parse([]string{
		"tcp://192.168.1.1:80",
		"tcp://10.0.0.5:443",  // <- this is the cache; should be excluded from worker AllowedIPs
		"tcp://172.16.0.0/24", // CIDR passes through
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	reg := acl.NewRegistry(entries)
	a := &registryAdapter{reg: reg, excludeCache: netip.MustParseAddr("10.0.0.5")}
	got := a.AllowedIPsCIDRs()
	want := []string{"172.16.0.0/24", "192.168.1.1/32"}
	if !equalStringSlice(got, want) {
		t.Errorf("AllowedIPsCIDRs = %v, want %v", got, want)
	}
}

func TestRegistryAdapter_NoExcludedHost(t *testing.T) {
	entries, err := acl.Parse([]string{"tcp://192.168.1.1:80"})
	if err != nil {
		t.Fatal(err)
	}
	reg := acl.NewRegistry(entries)
	// excludeCache set to a non-present IP; nothing should be excluded.
	a := &registryAdapter{reg: reg, excludeCache: netip.MustParseAddr("203.0.113.99")}
	got := a.AllowedIPsCIDRs()
	want := []string{"192.168.1.1/32"}
	if !equalStringSlice(got, want) {
		t.Errorf("AllowedIPsCIDRs = %v, want %v", got, want)
	}
}

func TestWithOrchestratorHost_AddsWhenMissing(t *testing.T) {
	orch := netip.MustParseAddr("100.64.0.1")
	prefixes := []netip.Prefix{
		netip.MustParsePrefix("192.168.1.1/32"),
	}
	got := withOrchestratorHost(prefixes, orch)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0] != netip.MustParsePrefix("100.64.0.1/32") {
		t.Errorf("got[0] = %v, want orchestrator /32", got[0])
	}
}

func TestWithOrchestratorHost_PreservesWhenPresent(t *testing.T) {
	orch := netip.MustParseAddr("100.64.0.1")
	prefixes := []netip.Prefix{
		netip.MustParsePrefix("100.64.0.1/32"),
		netip.MustParsePrefix("192.168.1.1/32"),
	}
	got := withOrchestratorHost(prefixes, orch)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (input unchanged)", len(got))
	}
}

func TestNextHostInPrefix_OverlayP30(t *testing.T) {
	overlay := netip.MustParsePrefix("100.64.0.0/30")
	orch := netip.MustParseAddr("100.64.0.1")
	got := nextHostInPrefix(overlay, orch)
	if got.String() != "100.64.0.2" {
		t.Errorf("nextHost = %v, want 100.64.0.2", got)
	}
}

func TestNextHostInPrefix_FallbackOutsidePrefix(t *testing.T) {
	overlay := netip.MustParsePrefix("100.64.0.0/30")
	// orch outside the overlay — nextHostInPrefix should fall back to
	// the first usable host in the prefix.
	orch := netip.MustParseAddr("10.0.0.99")
	got := nextHostInPrefix(overlay, orch)
	// .Next of .99 is .100 which isn't in the /30; fallback path
	// returns prefix.Masked().Addr().Next() = 100.64.0.1.
	if got.String() != "100.64.0.1" {
		t.Errorf("nextHost fallback = %v, want 100.64.0.1", got)
	}
}

func TestParseHostAddr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"10.99.0.1", "10.99.0.1"},
		{"10.99.0.1/32", "10.99.0.1"},
		{"100.64.0.1/30", "100.64.0.1"},
	}
	for _, tc := range cases {
		got, err := parseHostAddr(tc.in)
		if err != nil {
			t.Errorf("parseHostAddr(%q) err = %v", tc.in, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("parseHostAddr(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseHostAddr_RejectsGarbage(t *testing.T) {
	if _, err := parseHostAddr("not-an-addr"); err == nil {
		t.Error("parseHostAddr(garbage) should error")
	}
}

// TestACLSourceContractMatchesProvider verifies the wgboot.ACLSource
// interface stays structurally compatible with the linode provider's
// ACLSnapshotSource shape — adding a method to one without the other
// would silently break the cmd/fj-bellows shim. The package-private
// adapter satisfies both shapes by structural typing; this test
// pins the contract so adding methods on one side breaks compilation
// here too.
func TestACLSourceContractMatchesProvider(_ *testing.T) {
	// The shim in cmd/fj-bellows asserts both interfaces share
	// `AllowedIPsCIDRs() []string`. This test exists in this package
	// so that a future contract drift breaks an explicit test rather
	// than a sneaky interface conversion in main.go.
	var _ ACLSource = (*registryAdapter)(nil)
}

// realLookup smoke test: with a synthetic context cancellation we get
// the expected error path, not a panic.
func TestRealLookup_RespectsCancelledContext(t *testing.T) {
	lk := newRealLookup()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := lk.LookupHost(ctx, "example.com")
	if err == nil {
		t.Error("expected error on cancelled ctx")
	}
}

// equalStringSlice wraps slices.Equal so test call sites read naturally.
func equalStringSlice(a, b []string) bool {
	return slices.Equal(a, b)
}

// Silence unused-import warnings during early dev iterations.
var _ = slog.Default
