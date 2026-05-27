package cachegateway

import (
	"strings"
	"testing"

	"github.com/hstern/fj-bellows/internal/config"
)

// stockInputs returns a representative Inputs value the table tests
// share. Centralised so a future field addition only updates one site.
func stockInputs() Inputs {
	return Inputs{
		Tunnel: config.Tunnel{
			Routes: []string{"192.168.0.0/24"},
			LANEgress: []config.LANEgressRule{
				{From: config.EgressFromWorkerVPC, To: "192.168.0.2", Port: 53, Proto: "udp"},
				{From: config.EgressFromWorkerVPC, To: "192.168.0.2", Port: 53, Proto: "tcp"},
				{From: config.EgressFromWorkerVPC, To: "192.168.0.7", Port: 443, Proto: "tcp"},
			},
		},
		CacheVPCIP:         "10.0.0.1",
		WorkerVPCSubnet:    "10.0.0.0/24",
		LANNameserver:      "192.168.0.2",
		DNSForwardZones:    []string{"stern.ca"},
		LANGatewayPublicIP: "203.0.113.10",
		CachePublicIP:      "172.105.10.20",
		PSK:                "verysecret",
	}
}

func TestValidateRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Inputs)
		wantSub string
	}{
		{"no cache VPC IP", func(i *Inputs) { i.CacheVPCIP = "" }, "CacheVPCIP is required"},
		{"bad cache VPC IP", func(i *Inputs) { i.CacheVPCIP = "not-an-ip" }, "not a valid IP"},
		{"no worker VPC subnet", func(i *Inputs) { i.WorkerVPCSubnet = "" }, "WorkerVPCSubnet is required"},
		{"bad worker VPC subnet", func(i *Inputs) { i.WorkerVPCSubnet = "not-a-cidr" }, "not a valid CIDR"},
		{"no cache public IP", func(i *Inputs) { i.CachePublicIP = "" }, "CachePublicIP is required"},
		{"no LAN gateway public IP", func(i *Inputs) { i.LANGatewayPublicIP = "" }, "LANGatewayPublicIP is required"},
		{"no PSK", func(i *Inputs) { i.PSK = "" }, "PSK is required"},
		{"bad tunnel route", func(i *Inputs) { i.Tunnel.Routes = []string{"bad-cidr"} }, "Tunnel.Routes contains an invalid CIDR"},
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

func TestRenderUnbound_MarkersAndForwarders(t *testing.T) {
	got, err := RenderUnbound(stockInputs())
	if err != nil {
		t.Fatalf("RenderUnbound: %v", err)
	}
	wantSubstrings := []string{
		"interface: 10.0.0.1",
		"access-control: 10.0.0.0/24 allow",
		`local-data: "cache. IN A 10.0.0.1"`,
		`name: "stern.ca"`,
		"forward-addr: 192.168.0.2",
		`name: "."`,
		"forward-addr: 1.1.1.1",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in unbound.conf:\n%s", sub, got)
		}
	}
}

func TestRenderUnbound_NoForwardZones(t *testing.T) {
	in := stockInputs()
	in.DNSForwardZones = nil
	got, err := RenderUnbound(in)
	if err != nil {
		t.Fatalf("RenderUnbound: %v", err)
	}
	if strings.Contains(got, `name: "stern.ca"`) {
		t.Errorf("unexpected stern.ca forward when DNSForwardZones is empty")
	}
	// Default forwarders still present.
	if !strings.Contains(got, `name: "."`) {
		t.Errorf("default forward-zone missing")
	}
}

func TestRenderStrongSwanResponder_Shape(t *testing.T) {
	conf, secrets, err := RenderStrongSwanResponder(stockInputs())
	if err != nil {
		t.Fatalf("RenderStrongSwanResponder: %v", err)
	}
	wantConfSubstrings := []string{
		"conn fjb-cache-gateway",
		"keyexchange=ikev2",
		"left=172.105.10.20",
		"leftsubnet=10.0.0.0/24",
		"right=203.0.113.10",
		"rightsubnet=192.168.0.0/24",
		"authby=secret",
	}
	for _, sub := range wantConfSubstrings {
		if !strings.Contains(conf, sub) {
			t.Errorf("missing %q in ipsec.conf:\n%s", sub, conf)
		}
	}
	if !strings.Contains(secrets, `: PSK "verysecret"`) {
		t.Errorf("secrets line missing PSK marker:\n%s", secrets)
	}
}

func TestRenderStrongSwanInitiator_SymmetricSubnets(t *testing.T) {
	conf, secrets, err := RenderStrongSwanInitiator(stockInputs())
	if err != nil {
		t.Fatalf("RenderStrongSwanInitiator: %v", err)
	}
	// Initiator's left = LAN-side public IP; rightsubnet = worker VPC.
	wantSubstrings := []string{
		"auto=start", // initiator (vs responder's auto=add)
		"left=203.0.113.10",
		"leftsubnet=192.168.0.0/24",
		"right=172.105.10.20",
		"rightsubnet=10.0.0.0/24",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(conf, sub) {
			t.Errorf("missing %q in initiator ipsec.conf:\n%s", sub, conf)
		}
	}
	if !strings.Contains(secrets, "@fjb-lan @fjb-cache") {
		t.Errorf("secrets line uses initiator's own ID first:\n%s", secrets)
	}
}

func TestRenderStrongSwanResponder_MultipleRoutes(t *testing.T) {
	in := stockInputs()
	in.Tunnel.Routes = []string{"192.168.0.0/24", "10.10.0.0/16"}
	conf, _, err := RenderStrongSwanResponder(in)
	if err != nil {
		t.Fatalf("RenderStrongSwanResponder: %v", err)
	}
	if !strings.Contains(conf, "rightsubnet=192.168.0.0/24,10.10.0.0/16") {
		t.Errorf("multiple routes not joined with comma:\n%s", conf)
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
		"iptables -A FJB-FORWARD -s 10.0.0.0/24 -d 192.168.0.0/24 -j ACCEPT",
		"iptables -A FJB-FORWARD -p udp -s 10.0.0.0/24 -d 192.168.0.2 --dport 53 -j ACCEPT",
		"iptables -A FJB-FORWARD -p tcp -s 10.0.0.0/24 -d 192.168.0.7 --dport 443 -j ACCEPT",
		"iptables -A FJB-FORWARD -m state --state ESTABLISHED,RELATED -j ACCEPT",
		"iptables -A FJB-FORWARD -j DROP",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in iptables script:\n%s", sub, got)
		}
	}
}

func TestRenderCacheIPTables_EmptyLanEgress(t *testing.T) {
	in := stockInputs()
	in.Tunnel.LANEgress = nil
	got, err := RenderCacheIPTables(in)
	if err != nil {
		t.Fatalf("RenderCacheIPTables: %v", err)
	}
	if strings.Contains(got, "--dport") {
		t.Errorf("unexpected port-based rule when LANEgress is empty")
	}
	// Tunnel routes still rendered.
	if !strings.Contains(got, "iptables -A FJB-FORWARD -s 10.0.0.0/24 -d 192.168.0.0/24 -j ACCEPT") {
		t.Errorf("tunnel-route ACCEPT missing")
	}
}

func TestRenderLANNftables_AllowList(t *testing.T) {
	got, err := RenderLANNftables(stockInputs())
	if err != nil {
		t.Fatalf("RenderLANNftables: %v", err)
	}
	wantSubstrings := []string{
		"flush table inet fjb",
		"table inet fjb {",
		"chain forward {",
		"type filter hook forward",
		"ip saddr 10.0.0.0/24 ip daddr 192.168.0.2 udp dport 53 accept",
		"ip saddr 10.0.0.0/24 ip daddr 192.168.0.2 tcp dport 53 accept",
		"ip saddr 10.0.0.0/24 ip daddr 192.168.0.7 tcp dport 443 accept",
		"ip saddr 10.0.0.0/24 drop",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in nftables script:\n%s", sub, got)
		}
	}
}

func TestRenderLANNftables_NoLeakyDefaults(t *testing.T) {
	// The final "drop" must be scoped to the worker VPC source. We
	// don't want a blanket drop that could catch operator-authored
	// flows on the same host.
	got, err := RenderLANNftables(stockInputs())
	if err != nil {
		t.Fatalf("RenderLANNftables: %v", err)
	}
	if !strings.Contains(got, "ip saddr 10.0.0.0/24 drop") {
		t.Errorf("final drop is not scoped to worker VPC source:\n%s", got)
	}
	// Sanity: the policy is `accept` so unmatched non-worker traffic
	// passes through.
	if !strings.Contains(got, "policy accept;") {
		t.Errorf("default policy should be accept (we only filter worker VPC traffic):\n%s", got)
	}
}
