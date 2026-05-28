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
	return testFirewallWithTransportAndPort(cfg, mode, 0)
}

// testFirewallWithTransportAndPort lets a test override the WG listen port,
// for the FJB-89 configurability assertions. Zero falls back to the default
// inside synthSpecsForTransport.
func testFirewallWithTransportAndPort(cfg firewallConfig, mode string, wgListenPort int) *managedFirewall {
	return &managedFirewall{
		cfg: cfg,
		ipProbe: externalIPProbe{
			v4URL:  "https://test/v4",
			v6URL:  "https://test/v6",
			client: stubDoer{},
		},
		log:           slog.Default(),
		transportMode: mode,
		wgListenPort:  wgListenPort,
	}
}

func TestSynthSpecsForTransport(t *testing.T) {
	cases := []struct {
		name         string
		mode         string
		wgListenPort int
		wantCount    int
		wantProtos   []linodego.NetworkProtocol
		wantPorts    []string
	}{
		{
			name:       "default ssh (empty)",
			mode:       "",
			wantCount:  1,
			wantProtos: []linodego.NetworkProtocol{linodego.TCP},
			wantPorts:  []string{"22"},
		},
		{
			name:       "explicit ssh",
			mode:       transportSSHExplicit,
			wantCount:  1,
			wantProtos: []linodego.NetworkProtocol{linodego.TCP},
			wantPorts:  []string{"22"},
		},
		{
			name:       "cache-gateway default WG port",
			mode:       transportCacheGateway,
			wantCount:  1,
			wantProtos: []linodego.NetworkProtocol{linodego.UDP},
			wantPorts:  []string{"51820"},
		},
		{
			name:         "cache-gateway custom WG port",
			mode:         transportCacheGateway,
			wgListenPort: 51821,
			wantCount:    1,
			wantProtos:   []linodego.NetworkProtocol{linodego.UDP},
			wantPorts:    []string{"51821"},
		},
		{
			// Unrecognised mode falls back to SSH behaviour for safety.
			name:       "unknown mode falls back to ssh",
			mode:       "wireguard-mesh",
			wantCount:  1,
			wantProtos: []linodego.NetworkProtocol{linodego.TCP},
			wantPorts:  []string{"22"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := synthSpecsForTransport(tc.mode, tc.wgListenPort)
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

// TestBuildRuleSetCacheGatewayWG verifies the cache-gateway transport
// synthesizes a single ACCEPT rule per address family for udp/<wg listen
// port> (FJB-89), replacing the IPsec port set that lived here before.
//
//nolint:gocyclo // exhaustive single-site assertion of proto/ports/family + leftover-IPsec absence checks.
func TestBuildRuleSetCacheGatewayWG(t *testing.T) {
	fw := testFirewallWithTransport(firewallConfig{}, transportCacheGateway)
	rs, err := fw.buildRuleSet(context.Background(), []string{testCIDR1, testCIDRv6})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	// 1 spec × (1 v4 chunk + 1 v6 chunk) = 2 inbound rules.
	if len(rs.Inbound) != 2 {
		t.Fatalf("len(Inbound) = %d, want 2 (1 WG spec × 2 families)", len(rs.Inbound))
	}
	var v4Total, v6Total int
	for _, r := range rs.Inbound {
		if r.Action != fwActionAccept {
			t.Errorf("rule %q: action=%q, want ACCEPT", r.Label, r.Action)
		}
		if r.Protocol != linodego.UDP {
			t.Errorf("rule %q: proto=%s, want UDP", r.Label, r.Protocol)
		}
		if r.Ports != "51820" {
			t.Errorf("rule %q: ports=%q, want %q", r.Label, r.Ports, "51820")
		}
		v4Total += len(*r.Addresses.IPv4)
		v6Total += len(*r.Addresses.IPv6)
	}
	if v4Total != 1 || v6Total != 1 {
		t.Errorf("address bucketing: v4=%d v6=%d, want 1 + 1", v4Total, v6Total)
	}
	// No tcp/22 rule should exist in cache-gateway mode.
	// Likewise no leftover IPsec (udp/500, udp/4500, ESP) rules.
	for _, r := range rs.Inbound {
		if r.Protocol == linodego.TCP && r.Ports == "22" {
			t.Errorf("found legacy tcp/22 rule under cache-gateway: %+v", r)
		}
		if r.Protocol == linodego.UDP && (r.Ports == "500" || r.Ports == "4500") {
			t.Errorf("found stale IPsec rule under cache-gateway: %+v", r)
		}
		if r.Protocol == linodego.IPENCAP {
			t.Errorf("found stale ESP/IPENCAP rule under cache-gateway: %+v", r)
		}
	}
	// Default policies still applied.
	if rs.InboundPolicy != fwInboundDrop || rs.OutboundPolicy != fwActionAccept {
		t.Errorf("policies = %q/%q, want DROP/ACCEPT", rs.InboundPolicy, rs.OutboundPolicy)
	}
}

// TestBuildRuleSetCacheGatewayCustomWGPort verifies the WG listen port
// is plumbed through synthSpecsForTransport into the rendered ruleset.
func TestBuildRuleSetCacheGatewayCustomWGPort(t *testing.T) {
	const customPort = 51821
	fw := testFirewallWithTransportAndPort(firewallConfig{}, transportCacheGateway, customPort)
	rs, err := fw.buildRuleSet(context.Background(), []string{testCIDR1})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	if len(rs.Inbound) != 1 {
		t.Fatalf("len(Inbound) = %d, want 1", len(rs.Inbound))
	}
	r := rs.Inbound[0]
	if r.Protocol != linodego.UDP || r.Ports != "51821" {
		t.Errorf("rule = (%s, %q), want (UDP, %q)", r.Protocol, r.Ports, "51821")
	}
}

// TestBuildRuleSetCacheGatewayLabelsUnique — the WG spec shares the same
// CIDR set across families; rule labels must remain unique within the
// firewall.
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
	// Spot-check the WG label stem is present.
	found := false
	for label := range labels {
		if containsSubstring(label, "wg") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no rule label contains %q; got labels %v", "wg", labels)
	}
}

// TestBuildRuleSetCacheGatewayChunking — 300 v4 entries should chunk into
// 2 rules for the one WG spec, so total 1 spec × 2 chunks = 2 v4 rules
// (no v6).
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
	// One WG spec × ceil(300/255) = 2 v4 rules, 0 v6.
	if len(rs.Inbound) != 2 {
		t.Fatalf("len(Inbound) = %d, want 2 (1 WG spec × 2 v4 chunks)", len(rs.Inbound))
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

// TestBuildRuleSetCacheGatewayRejectsWhenOverCap — even with the single
// WG spec, an oversized extras list still trips the 25-rule cap. The
// pre-flight check fires before any per-rule work.
func TestBuildRuleSetCacheGatewayRejectsWhenOverCap(t *testing.T) {
	// 1 WG spec × 1 v4 chunk + 25 extras = 26 inbound rules, over the cap.
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
