package cachegateway

import (
	"net/netip"
	"strings"
	"testing"
)

// stockInputs returns a representative Inputs value that covers a
// typical ACL: orchestrator overlay /32, the operator's LAN DNS server
// as a literal /32, and a CIDR-shaped destination (the Forgejo server's
// LAN block).
func stockInputs() Inputs {
	return Inputs{
		CacheVPCIP:         "10.0.0.1",
		WorkerVPCSubnet:    "10.0.0.0/24",
		OverlayPrefix:      netip.MustParsePrefix("100.64.0.0/30"),
		OrchestratorWGAddr: netip.MustParseAddr("100.64.0.1"),
		CacheWGAddr:        netip.MustParseAddr("100.64.0.2"),
		AllowedIPs: []netip.Prefix{
			netip.MustParsePrefix("100.64.0.1/32"),  // orchestrator
			netip.MustParsePrefix("192.168.0.2/32"), // LAN DNS
			netip.MustParsePrefix("192.168.10.0/24"),
		},
	}
}

func TestValidateRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Inputs)
		wantSub string
	}{
		{"bad cache VPC IP", func(i *Inputs) { i.CacheVPCIP = "not-an-ip" }, "not a valid IP"},
		{"no worker VPC subnet", func(i *Inputs) { i.WorkerVPCSubnet = "" }, "WorkerVPCSubnet is required"},
		{"bad worker VPC subnet", func(i *Inputs) { i.WorkerVPCSubnet = "not-a-cidr" }, "not a valid CIDR"},
		{"invalid AllowedIPs entry", func(i *Inputs) {
			i.AllowedIPs = []netip.Prefix{{}}
		}, "AllowedIPs contains an invalid prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := stockInputs()
			tc.mutate(&in)
			err := in.validate()
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestRenderCacheIPTables_AllowList(t *testing.T) {
	got, err := RenderCacheIPTables(stockInputs())
	if err != nil {
		t.Fatalf("RenderCacheIPTables: %v", err)
	}
	wantSubstrings := []string{
		"#!/usr/bin/env bash",
		"set -euo pipefail",
		"sysctl -w net.ipv4.ip_forward=1",
		"iptables -N FJB-FORWARD",
		"iptables -F FJB-FORWARD",
		"iptables -A FJB-FORWARD -m state --state ESTABLISHED,RELATED -j ACCEPT",
		"iptables -A FJB-FORWARD -s 10.0.0.0/24 -d 100.64.0.1/32 -o wg0 -j ACCEPT",
		"iptables -A FJB-FORWARD -s 10.0.0.0/24 -d 192.168.0.2/32 -o wg0 -j ACCEPT",
		"iptables -A FJB-FORWARD -s 10.0.0.0/24 -d 192.168.10.0/24 -o wg0 -j ACCEPT",
		// FJB-92: orchestrator → worker VPC reverse direction.
		"iptables -A FJB-FORWARD -s 100.64.0.1/32 -d 10.0.0.0/24 -i wg0 -o eth1 -j ACCEPT",
		"iptables -A FJB-FORWARD -j DROP",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in iptables script:\n%s", sub, got)
		}
	}
	// Make sure the obsolete strongSwan/lan_egress rule shapes don't
	// leak back in.
	notWanted := []string{
		"--dport",
		"-p tcp",
		"-p udp",
	}
	for _, sub := range notWanted {
		if strings.Contains(got, sub) {
			t.Errorf("unexpected legacy substring %q in iptables script:\n%s", sub, got)
		}
	}
}

// An empty ACL is permitted by the renderer — the chain still installs
// (ESTABLISHED,RELATED + final DROP), but no ACCEPT rules are emitted.
// The orchestrator-side composition layer (FJB-90) is responsible for
// ensuring at least the orchestrator /32 is in the union; this renderer
// is intentionally pure-mechanical.
func TestRenderCacheIPTables_EmptyAllowedIPs(t *testing.T) {
	in := stockInputs()
	in.AllowedIPs = nil
	got, err := RenderCacheIPTables(in)
	if err != nil {
		t.Fatalf("RenderCacheIPTables: %v", err)
	}
	if strings.Contains(got, "-o wg0 -j ACCEPT") {
		t.Errorf("unexpected ACCEPT rule when AllowedIPs is empty:\n%s", got)
	}
	// The skeleton (sysctl + chain create + ESTABLISHED,RELATED + DROP)
	// still renders.
	for _, sub := range []string{
		"sysctl -w net.ipv4.ip_forward=1",
		"iptables -A FJB-FORWARD -m state --state ESTABLISHED,RELATED -j ACCEPT",
		"iptables -A FJB-FORWARD -j DROP",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in empty-ACL iptables script:\n%s", sub, got)
		}
	}
}

// FJB-92: the orchestrator → worker VPC reverse rule emits whenever a
// valid OrchestratorWGAddr is set; the dispatcher's netstack dials worker
// IPs through wg0 and the cache must forward them onto eth1.
func TestRenderCacheIPTables_OrchestratorToWorkerReverseRule(t *testing.T) {
	in := stockInputs()
	// Drop AllowedIPs so the only direction-bearing line is the new one;
	// the skeleton (state, DROP) still renders.
	in.AllowedIPs = nil
	got, err := RenderCacheIPTables(in)
	if err != nil {
		t.Fatalf("RenderCacheIPTables: %v", err)
	}
	want := "iptables -A FJB-FORWARD -s 100.64.0.1/32 -d 10.0.0.0/24 -i wg0 -o eth1 -j ACCEPT"
	if !strings.Contains(got, want) {
		t.Errorf("missing reverse-direction ACCEPT rule %q in script:\n%s", want, got)
	}
}

// FJB-92: when OrchestratorWGAddr is the zero value, the reverse rule is
// omitted. Callers that don't know the orchestrator overlay address yet
// (e.g. early bring-up paths or non-default test cases) still get a
// valid script.
func TestRenderCacheIPTables_OmitsReverseRuleWhenOrchAddrInvalid(t *testing.T) {
	in := stockInputs()
	in.OrchestratorWGAddr = netip.Addr{}
	got, err := RenderCacheIPTables(in)
	if err != nil {
		t.Fatalf("RenderCacheIPTables: %v", err)
	}
	if strings.Contains(got, "-i wg0 -o eth1") {
		t.Errorf("reverse rule should be omitted when OrchestratorWGAddr is invalid:\n%s", got)
	}
}

// IPv6 prefixes flow through unchanged (iptables-ipv6 / ip6tables is
// out of scope here — the script targets v4 explicitly — but the
// renderer doesn't reject v6 inputs; v6 enforcement is a future
// concern. For now, document that v6 prefixes pass through as-is.)
func TestRenderCacheIPTables_IPv6PrefixPassesThrough(t *testing.T) {
	in := stockInputs()
	in.AllowedIPs = append(in.AllowedIPs, netip.MustParsePrefix("2001:db8::/32"))
	got, err := RenderCacheIPTables(in)
	if err != nil {
		t.Fatalf("RenderCacheIPTables: %v", err)
	}
	if !strings.Contains(got, "-d 2001:db8::/32 -o wg0 -j ACCEPT") {
		t.Errorf("v6 prefix not rendered as iptables -d arg (renderer should pass through):\n%s", got)
	}
}
