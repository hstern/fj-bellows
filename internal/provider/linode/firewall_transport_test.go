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
			// FJB-99 Phase C — cache-gateway emits udp/<wg> + tcp/22.
			// tcp/22 stays open as the operator debug break-glass; the
			// WG tunnel remains the load-bearing access path.
			name:       "cache-gateway default WG port",
			mode:       transportCacheGateway,
			wantCount:  2,
			wantProtos: []linodego.NetworkProtocol{linodego.UDP, linodego.TCP},
			wantPorts:  []string{"51820", "22"},
		},
		{
			name:         "cache-gateway custom WG port",
			mode:         transportCacheGateway,
			wgListenPort: 51821,
			wantCount:    2,
			wantProtos:   []linodego.NetworkProtocol{linodego.UDP, linodego.TCP},
			wantPorts:    []string{"51821", "22"},
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
// synthesizes ACCEPT rules per address family for udp/<wg listen port>
// (FJB-89) AND tcp/22 (FJB-99 Phase C debug break-glass).
//
//nolint:gocyclo // exhaustive single-site assertion of proto/ports/family + leftover-IPsec absence checks.
func TestBuildRuleSetCacheGatewayWG(t *testing.T) {
	fw := testFirewallWithTransport(firewallConfig{}, transportCacheGateway)
	rs, err := fw.buildRuleSet(context.Background(), []string{testCIDR1, testCIDRv6})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	// 2 specs (WG + ssh-debug) × (1 v4 chunk + 1 v6 chunk) = 4 inbound rules.
	if len(rs.Inbound) != 4 {
		t.Fatalf("len(Inbound) = %d, want 4 (2 specs × 2 families)", len(rs.Inbound))
	}
	var wgSeen, sshSeen int
	for _, r := range rs.Inbound {
		if r.Action != fwActionAccept {
			t.Errorf("rule %q: action=%q, want ACCEPT", r.Label, r.Action)
		}
		switch {
		case r.Protocol == linodego.UDP && r.Ports == "51820":
			wgSeen++
		case r.Protocol == linodego.TCP && r.Ports == "22":
			sshSeen++
		default:
			t.Errorf("unexpected rule %q: proto=%s, ports=%q", r.Label, r.Protocol, r.Ports)
		}
	}
	if wgSeen != 2 || sshSeen != 2 {
		t.Errorf("rule counts: wg=%d ssh=%d, want 2 each (1 v4 + 1 v6)", wgSeen, sshSeen)
	}
	// No leftover IPsec (udp/500, udp/4500, ESP) rules.
	for _, r := range rs.Inbound {
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
// Now also asserts the tcp/22 debug break-glass (FJB-99 Phase C) is
// present alongside the custom WG port.
func TestBuildRuleSetCacheGatewayCustomWGPort(t *testing.T) {
	const customPort = 51821
	fw := testFirewallWithTransportAndPort(firewallConfig{}, transportCacheGateway, customPort)
	rs, err := fw.buildRuleSet(context.Background(), []string{testCIDR1})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	if len(rs.Inbound) != 2 {
		t.Fatalf("len(Inbound) = %d, want 2 (WG + ssh-debug)", len(rs.Inbound))
	}
	var wgSeen, sshSeen bool
	for _, r := range rs.Inbound {
		switch {
		case r.Protocol == linodego.UDP && r.Ports == "51821":
			wgSeen = true
		case r.Protocol == linodego.TCP && r.Ports == "22":
			sshSeen = true
		}
	}
	if !wgSeen || !sshSeen {
		t.Errorf("rules wgSeen=%v sshSeen=%v, want both true", wgSeen, sshSeen)
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
	// 2 specs (WG + ssh-debug) × ceil(300/255) = 4 v4 rules, 0 v6.
	if len(rs.Inbound) != 4 {
		t.Fatalf("len(Inbound) = %d, want 4 (2 specs × 2 v4 chunks)", len(rs.Inbound))
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
