package cachegateway

import (
	"fmt"
	"strings"
)

// RenderCacheIPTables returns a shell script that, run as root on the
// cache nanode, enables IP forwarding and installs the FORWARD chain
// rules that permit only worker → routed-CIDR + the configured
// lan_egress tuples. Idempotent: re-running flushes the fj-bellows
// chain and reapplies.
//
// The output is bash (`#!/usr/bin/env bash`, `set -euo pipefail`) so
// operators can drop it into /usr/local/sbin/fjb-iptables.sh and
// arrange to run it at boot (systemd unit, /etc/rc.local, etc.).
//
// Consumed fields: WorkerVPCSubnet, Tunnel.Routes, Tunnel.LANEgress.
func RenderCacheIPTables(in Inputs) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString("# fjb-iptables.sh — fj-bellows cache nanode (FJB-71)\n")
	b.WriteString("# Generated; do not edit by hand. Idempotent (flushes + reinstalls FJB chain).\n")
	b.WriteString("set -euo pipefail\n\n")

	b.WriteString("# Kernel IP forwarding (worker VPC → IPsec tunnel).\n")
	b.WriteString("sysctl -w net.ipv4.ip_forward=1 >/dev/null\n")
	b.WriteString("install -d /etc/sysctl.d\n")
	b.WriteString("printf 'net.ipv4.ip_forward = 1\\n' > /etc/sysctl.d/99-fjb-ip-forward.conf\n\n")

	b.WriteString("# fjb-managed FORWARD chain. Hooks into the FORWARD policy chain;\n")
	b.WriteString("# everything not explicitly accepted falls back to the policy default.\n")
	b.WriteString("iptables -N FJB-FORWARD 2>/dev/null || true\n")
	b.WriteString("iptables -F FJB-FORWARD\n")
	b.WriteString("iptables -C FORWARD -j FJB-FORWARD 2>/dev/null || iptables -I FORWARD 1 -j FJB-FORWARD\n\n")

	// Allow worker VPC → each tunnel-routed CIDR.
	b.WriteString("# Worker VPC → tunnel-routed CIDRs (FJB-72 transport.tunnel.routes).\n")
	for _, route := range in.Tunnel.Routes {
		fmt.Fprintf(&b, "iptables -A FJB-FORWARD -s %s -d %s -j ACCEPT\n", in.WorkerVPCSubnet, route)
	}
	if len(in.Tunnel.Routes) > 0 {
		b.WriteString("\n")
	}

	// Per-rule lan_egress accepts (matches the LAN-side outbound
	// firewall — defence in depth; cache iptables only forwards what
	// matches an explicit accept, even though the tunnel SA itself
	// already negotiates the right-subnet).
	if len(in.Tunnel.LANEgress) > 0 {
		b.WriteString("# Outbound allow-list (FJB-72 transport.tunnel.lan_egress).\n")
		for _, rule := range in.Tunnel.LANEgress {
			fmt.Fprintf(&b, "iptables -A FJB-FORWARD -p %s -s %s -d %s --dport %d -j ACCEPT\n",
				strings.ToLower(rule.Proto), in.WorkerVPCSubnet, rule.To, rule.Port)
		}
		b.WriteString("\n")
	}

	// Return path (established/related).
	b.WriteString("# Return traffic for the accepted flows.\n")
	b.WriteString("iptables -A FJB-FORWARD -m state --state ESTABLISHED,RELATED -j ACCEPT\n\n")

	// Default deny for the chain so operator's policy defaults don't
	// inadvertently permit things we haven't whitelisted.
	b.WriteString("# Anything not explicitly accepted: drop.\n")
	b.WriteString("iptables -A FJB-FORWARD -j DROP\n")

	return b.String(), nil
}
