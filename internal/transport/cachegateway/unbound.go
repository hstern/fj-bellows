package cachegateway

import (
	"fmt"
	"strings"
)

// RenderUnbound returns the /etc/unbound/unbound.conf content for the
// cache nanode. The resolver binds the cache's VPC IP, answers the
// short name "cache" with that same IP (so workers can `docker pull
// cache:5000/...`), forwards DNSForwardZones to LANNameserver over the
// IPsec tunnel, and defers everything else to public resolvers.
//
// Consumed fields: CacheVPCIP, WorkerVPCSubnet, LANNameserver,
// DNSForwardZones.
func RenderUnbound(in Inputs) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# unbound.conf — fj-bellows cache nanode (FJB-71)\n")
	b.WriteString("# Resolver for the worker VPC. Generated; do not edit by hand.\n\n")
	b.WriteString("server:\n")
	b.WriteString("  verbosity: 1\n")
	fmt.Fprintf(&b, "  interface: %s\n", in.CacheVPCIP)
	b.WriteString("  port: 53\n")
	fmt.Fprintf(&b, "  access-control: %s allow\n", in.WorkerVPCSubnet)
	b.WriteString("  access-control: 127.0.0.0/8 allow\n")
	b.WriteString("  access-control: 0.0.0.0/0 refuse\n")
	b.WriteString("  do-ip4: yes\n")
	b.WriteString("  do-ip6: no\n")
	b.WriteString("  do-udp: yes\n")
	b.WriteString("  do-tcp: yes\n")
	b.WriteString("  hide-identity: yes\n")
	b.WriteString("  hide-version: yes\n")
	b.WriteString("  qname-minimisation: yes\n")
	b.WriteString("  harden-glue: yes\n")
	b.WriteString("  harden-dnssec-stripped: yes\n")
	b.WriteString("  prefetch: yes\n\n")

	// Local-data: "cache" → cache VPC IP. Workers reach the registry by
	// dialing "cache:5000" and TLS-validate against the fjb-managed CA
	// they were given at cloud-init time.
	b.WriteString("  # short-name → cache VPC IP\n")
	fmt.Fprintf(&b, "  local-zone: \"cache.\" static\n")
	fmt.Fprintf(&b, "  local-data: \"cache. IN A %s\"\n\n", in.CacheVPCIP)

	// Forward zones over the IPsec tunnel to the LAN DNS.
	for _, zone := range in.DNSForwardZones {
		fmt.Fprintf(&b, "forward-zone:\n")
		fmt.Fprintf(&b, "  name: %q\n", zone)
		fmt.Fprintf(&b, "  forward-addr: %s\n\n", in.LANNameserver)
	}

	// Default forwarders for everything else.
	b.WriteString("forward-zone:\n")
	b.WriteString("  name: \".\"\n")
	b.WriteString("  forward-addr: 1.1.1.1\n")
	b.WriteString("  forward-addr: 8.8.8.8\n")
	return b.String(), nil
}
