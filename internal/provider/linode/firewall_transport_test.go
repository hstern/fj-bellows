package linode

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"testing"

	"github.com/linode/linodego"
)

// testFirewallWithTransport mirrors testFirewall but stamps a transport mode.
func testFirewallWithTransport(cfg firewallConfig, mode string) *managedFirewall {
	return &managedFirewall{
		cfg: cfg,
		ipProbe: externalIPProbe{
			v4URL:  "https://test/v4",
			v6URL:  "https://test/v6",
			client: stubDoer{},
		},
		log:           slog.Default(),
		transportMode: mode,
	}
}

func TestSynthSpecsForTransport(t *testing.T) {
	cases := []struct {
		mode       string
		wantCount  int
		wantProtos []linodego.NetworkProtocol
		wantPorts  []string
	}{
		{
			mode:       "",
			wantCount:  1,
			wantProtos: []linodego.NetworkProtocol{linodego.TCP},
			wantPorts:  []string{"22"},
		},
		{
			mode:       transportSSHExplicit,
			wantCount:  1,
			wantProtos: []linodego.NetworkProtocol{linodego.TCP},
			wantPorts:  []string{"22"},
		},
		{
			mode:       transportCacheGateway,
			wantCount:  3,
			wantProtos: []linodego.NetworkProtocol{linodego.UDP, linodego.UDP, linodego.IPENCAP},
			wantPorts:  []string{"500", "4500", ""},
		},
		{
			// Unrecognised mode falls back to SSH behaviour for safety.
			mode:       "wireguard-mesh",
			wantCount:  1,
			wantProtos: []linodego.NetworkProtocol{linodego.TCP},
			wantPorts:  []string{"22"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			got := synthSpecsForTransport(tc.mode)
			if len(got) != tc.wantCount {
				t.Fatalf("len=%d, want %d", len(got), tc.wantCount)
			}
			for i, s := range got {
				if s.proto != tc.wantProtos[i] || s.ports != tc.wantPorts[i] {
					t.Errorf("spec[%d] = (%s, %q), want (%s, %q)",
						i, s.proto, s.ports, tc.wantProtos[i], tc.wantPorts[i])
				}
			}
		})
	}
}

// TestBuildRuleSetCacheGatewayIPsec verifies the cache-gateway transport
// synthesizes ACCEPT rules for IPsec (udp/500 + udp/4500 + ESP) instead of
// the legacy tcp/22.
//
//nolint:gocyclo // bookkeeping across 3 specs × 2 families; intentional for one assertion site.
func TestBuildRuleSetCacheGatewayIPsec(t *testing.T) {
	fw := testFirewallWithTransport(firewallConfig{}, transportCacheGateway)
	rs, err := fw.buildRuleSet(context.Background(), []string{testCIDR1, testCIDRv6})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	// 3 specs × (1 v4 chunk + 1 v6 chunk) = 6 inbound rules.
	if len(rs.Inbound) != 6 {
		t.Fatalf("len(Inbound) = %d, want 6 (3 specs × 2 families)", len(rs.Inbound))
	}
	// Group rules by (proto, ports) to verify each IPsec spec produced
	// exactly the expected family split.
	type key struct {
		proto linodego.NetworkProtocol
		ports string
	}
	families := map[key]struct{ v4Total, v6Total int }{}
	for _, r := range rs.Inbound {
		if r.Action != fwActionAccept {
			t.Errorf("rule %q: action=%q, want ACCEPT", r.Label, r.Action)
		}
		k := key{r.Protocol, r.Ports}
		f := families[k]
		f.v4Total += len(*r.Addresses.IPv4)
		f.v6Total += len(*r.Addresses.IPv6)
		families[k] = f
	}
	wantKeys := []key{
		{linodego.UDP, "500"},
		{linodego.UDP, "4500"},
		{linodego.IPENCAP, ""},
	}
	for _, k := range wantKeys {
		f, ok := families[k]
		if !ok {
			t.Errorf("missing IPsec spec %v in rules", k)
			continue
		}
		if f.v4Total != 1 || f.v6Total != 1 {
			t.Errorf("spec %v: v4=%d v6=%d, want 1 + 1", k, f.v4Total, f.v6Total)
		}
	}
	// No tcp/22 rule should exist in cache-gateway mode.
	for _, r := range rs.Inbound {
		if r.Protocol == linodego.TCP && r.Ports == "22" {
			t.Errorf("found legacy tcp/22 rule under cache-gateway: %+v", r)
		}
	}
	// Default policies still applied.
	if rs.InboundPolicy != fwInboundDrop || rs.OutboundPolicy != fwActionAccept {
		t.Errorf("policies = %q/%q, want DROP/ACCEPT", rs.InboundPolicy, rs.OutboundPolicy)
	}
}

// TestBuildRuleSetCacheGatewayLabelsUnique — three specs share the same
// CIDR set; rule labels must remain unique within the firewall.
func TestBuildRuleSetCacheGatewayLabelsUnique(t *testing.T) {
	fw := testFirewallWithTransport(firewallConfig{}, transportCacheGateway)
	rs, err := fw.buildRuleSet(context.Background(), []string{testCIDR1, testCIDRv6})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	labels := map[string]int{}
	for _, r := range rs.Inbound {
		labels[r.Label]++
	}
	for label, n := range labels {
		if n != 1 {
			t.Errorf("label %q appears %d times, want 1", label, n)
		}
	}
	// Spot-check the label scheme matches the spec stems.
	wantSubstrings := []string{"ipsec-ike", "ipsec-natt", "ipsec-esp"}
	for _, sub := range wantSubstrings {
		found := false
		for label := range labels {
			if containsSubstring(label, sub) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no rule label contains %q", sub)
		}
	}
}

// TestBuildRuleSetCacheGatewayChunking — 300 v4 entries should chunk into
// 2 rules per spec, so total 3 specs × 2 chunks = 6 v4 rules (no v6).
func TestBuildRuleSetCacheGatewayChunking(t *testing.T) {
	cidrs := make([]string, 300)
	for i := range cidrs {
		// Spread across /24s so no two are equal; we don't dedupe here.
		cidrs[i] = ipv4CIDR(i)
	}
	fw := testFirewallWithTransport(firewallConfig{}, transportCacheGateway)
	rs, err := fw.buildRuleSet(context.Background(), cidrs)
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	// Each spec produces 2 v4 rules (ceil(300/255)) and 0 v6.
	if len(rs.Inbound) != 6 {
		t.Fatalf("len(Inbound) = %d, want 6 (3 specs × 2 v4 chunks)", len(rs.Inbound))
	}
}

// TestBuildRuleSetCacheGatewayHonoursPolicyOverrides — operator's
// inbound_policy / outbound_policy still apply under cache-gateway.
func TestBuildRuleSetCacheGatewayHonoursPolicyOverrides(t *testing.T) {
	cfg := firewallConfig{
		InboundPolicy:  fwActionAccept, // weird but operator's choice
		OutboundPolicy: fwInboundDrop,
	}
	fw := testFirewallWithTransport(cfg, transportCacheGateway)
	rs, err := fw.buildRuleSet(context.Background(), []string{testCIDR1})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	if rs.InboundPolicy != fwActionAccept {
		t.Errorf("InboundPolicy = %q, want %q", rs.InboundPolicy, fwActionAccept)
	}
	if rs.OutboundPolicy != fwInboundDrop {
		t.Errorf("OutboundPolicy = %q, want %q", rs.OutboundPolicy, fwInboundDrop)
	}
}

// TestBuildRuleSetCacheGatewayRejectsWhenOverCap — IPsec mode tripples the
// synth rule count, so the 25-rule cap is hit sooner with extras.
func TestBuildRuleSetCacheGatewayRejectsWhenOverCap(t *testing.T) {
	// 8 extras + 3 IPsec specs × 1 v4 chunk = 11 inbound rules. Fine.
	// But 25 extras + 3 specs = 28, over the cap.
	extras := make([]extraRule, 25)
	for i := range extras {
		extras[i] = extraRule{
			Label: "x", Action: fwActionAccept, Protocol: fwProtoTCP, Ports: "8080",
			Addresses: []string{testCIDR1},
		}
	}
	cfg := firewallConfig{ExtraInbound: extras}
	fw := testFirewallWithTransport(cfg, transportCacheGateway)
	_, err := fw.buildRuleSet(context.Background(), []string{testCIDR1})
	if err == nil {
		t.Fatal("buildRuleSet: want over-cap error, got nil")
	}
	if !containsSubstring(err.Error(), "exceeds Linode's per-firewall cap") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestBuildRuleSetSSHExplicitMatchesDefault — operator-supplied "ssh"
// produces an identical ruleset to the default empty mode.
func TestBuildRuleSetSSHExplicitMatchesDefault(t *testing.T) {
	cidrs := []string{testCIDR1, testCIDRv6}
	def := testFirewallWithTransport(firewallConfig{}, "")
	exp := testFirewallWithTransport(firewallConfig{}, transportSSHExplicit)
	rsDef, err := def.buildRuleSet(context.Background(), cidrs)
	if err != nil {
		t.Fatal(err)
	}
	rsExp, err := exp.buildRuleSet(context.Background(), cidrs)
	if err != nil {
		t.Fatal(err)
	}
	defLabels := make([]string, 0, len(rsDef.Inbound))
	for _, r := range rsDef.Inbound {
		defLabels = append(defLabels, r.Label)
	}
	expLabels := make([]string, 0, len(rsExp.Inbound))
	for _, r := range rsExp.Inbound {
		expLabels = append(expLabels, r.Label)
	}
	if !slices.Equal(defLabels, expLabels) {
		t.Errorf("explicit ssh labels diverged from default:\n  default = %v\n  explicit = %v",
			defLabels, expLabels)
	}
}

// containsSubstring is a tiny helper to avoid importing strings just for one
// Contains call inside table tests.
func containsSubstring(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ipv4CIDR returns a unique /32 CIDR for chunking tests. Caller passes 0..N.
func ipv4CIDR(i int) string {
	a := 10
	b := byte((i >> 16) & 0xff)
	c := byte((i >> 8) & 0xff)
	d := byte(i & 0xff)
	return fmt.Sprintf("%d.%d.%d.%d/32", a, b, c, d)
}
