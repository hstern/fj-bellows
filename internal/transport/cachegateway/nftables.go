package cachegateway

import (
	"fmt"
	"strings"
)

// RenderLANNftables returns the nft(8)-compatible script that installs
// the LAN-side outbound filter. The filter matches traffic emerging
// from the IPsec tunnel (sourced from the worker VPC subnet) and
// permits only the (proto, to, port) tuples in Tunnel.LANEgress.
//
// Defence in depth alongside the cache iptables FORWARD chain (FJB-71):
// even if a compromised cache or a misconfigured tunnel proposal leaks
// traffic, the LAN-side filter still blocks it.
//
// Output is a `nft -f -` script. Operator pipes it into nft on the
// LAN-side host: `cachegateway-lan-rules | sudo nft -f -`.
//
// Consumed fields: WorkerVPCSubnet, Tunnel.LANEgress.
func RenderLANNftables(in Inputs) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# nft script — fj-bellows LAN-side outbound filter (FJB-73)\n")
	b.WriteString("# Generated; do not edit by hand. Apply with: nft -f <thisfile>\n\n")

	// Atomic replacement via a dedicated table so we don't touch any
	// operator-authored rules in `filter`, `nat`, or other tables.
	b.WriteString("# Replace the fjb table atomically.\n")
	b.WriteString("flush table inet fjb 2>/dev/null || true\n")
	b.WriteString("delete table inet fjb 2>/dev/null || true\n\n")

	b.WriteString("table inet fjb {\n")
	b.WriteString("  chain forward {\n")
	b.WriteString("    type filter hook forward priority 0; policy accept;\n\n")

	// Match the worker VPC source. Anything from worker VPC that isn't
	// explicitly allowed below gets dropped at the end of the chain.
	fmt.Fprintf(&b, "    # Worker VPC source: %s\n", in.WorkerVPCSubnet)
	if len(in.Tunnel.LANEgress) > 0 {
		b.WriteString("    # Outbound allow-list (FJB-72 transport.tunnel.lan_egress).\n")
		for _, rule := range in.Tunnel.LANEgress {
			fmt.Fprintf(&b,
				"    ip saddr %s ip daddr %s %s dport %d accept\n",
				in.WorkerVPCSubnet, rule.To, strings.ToLower(rule.Proto), rule.Port,
			)
		}
		b.WriteString("\n")
	}

	// Return path.
	b.WriteString("    # Return traffic for accepted flows.\n")
	fmt.Fprintf(&b, "    ip daddr %s ct state established,related accept\n\n", in.WorkerVPCSubnet)

	// Anything else from the worker VPC: drop.
	fmt.Fprintf(&b, "    # Anything else from the worker VPC: drop.\n")
	fmt.Fprintf(&b, "    ip saddr %s drop\n", in.WorkerVPCSubnet)
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String(), nil
}
