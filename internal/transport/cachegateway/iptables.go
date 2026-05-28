package cachegateway

import (
	"fmt"
	"strings"
)

// RenderCacheIPTables returns a shell script that, run as root on the
// cache nanode, enables IP forwarding and installs the FORWARD chain
// rules that permit worker-VPC → AllowedIPs traffic out wg0. Idempotent:
// re-running flushes the fj-bellows chain and reapplies.
//
// The output is bash (`#!/usr/bin/env bash`, `set -euo pipefail`) so
// operators can drop it into /usr/local/sbin/fjb-iptables.sh and
// arrange to run it at boot (systemd unit, /etc/rc.local, etc.).
//
// Rule shape: one ACCEPT per AllowedIPs prefix, matching
// `-s <worker-vpc> -d <prefix> -o wg0`. Anything not matching the
// allow-list (or the ESTABLISHED,RELATED return path) is dropped by
// the chain's explicit final rule.
//
// Consumed fields: WorkerVPCSubnet, AllowedIPs.
func RenderCacheIPTables(in Inputs) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString("# fjb-iptables.sh — fj-bellows cache nanode (FJB-71, FJB-86)\n")
	b.WriteString("# Generated; do not edit by hand. Idempotent (flushes + reinstalls FJB chain).\n")
	b.WriteString("set -euo pipefail\n\n")

	b.WriteString("# Kernel IP forwarding (worker VPC → WG tunnel).\n")
	b.WriteString("sysctl -w net.ipv4.ip_forward=1 >/dev/null\n")
	b.WriteString("install -d /etc/sysctl.d\n")
	b.WriteString("printf 'net.ipv4.ip_forward = 1\\n' > /etc/sysctl.d/99-fjb-ip-forward.conf\n\n")

	b.WriteString("# fjb-managed FORWARD chain. Hooks into the FORWARD policy chain;\n")
	b.WriteString("# everything not explicitly accepted falls back to the chain's\n")
	b.WriteString("# explicit final DROP.\n")
	b.WriteString("iptables -N FJB-FORWARD 2>/dev/null || true\n")
	b.WriteString("iptables -F FJB-FORWARD\n")
	b.WriteString("iptables -C FORWARD -j FJB-FORWARD 2>/dev/null || iptables -I FORWARD 1 -j FJB-FORWARD\n\n")

	// Return path (established/related) first so reply traffic to
	// already-accepted flows doesn't have to re-match the prefix set.
	b.WriteString("# Return traffic for accepted flows.\n")
	b.WriteString("iptables -A FJB-FORWARD -m state --state ESTABLISHED,RELATED -j ACCEPT\n\n")

	// Allow worker VPC → each AllowedIPs prefix out wg0. This is the
	// ACL-derived union (FJB-82, composed by FJB-90).
	if len(in.AllowedIPs) > 0 {
		b.WriteString("# Worker VPC → ACL AllowedIPs union (FJB-86).\n")
		for _, p := range in.AllowedIPs {
			fmt.Fprintf(&b,
				"iptables -A FJB-FORWARD -s %s -d %s -o wg0 -j ACCEPT\n",
				in.WorkerVPCSubnet, p.String())
		}
		b.WriteString("\n")
	}

	// Default deny for the chain so operator's policy defaults don't
	// inadvertently permit things we haven't whitelisted.
	b.WriteString("# Anything not explicitly accepted: drop.\n")
	b.WriteString("iptables -A FJB-FORWARD -j DROP\n")

	return b.String(), nil
}
