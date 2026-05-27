package cachegateway

import (
	"fmt"
	"strings"
)

// LocalDataOverride is an authoritative-local-data row for unbound.
// Used under cache-as-gateway (WG path) to rewrite specific LAN
// hostnames to the orchestrator's WG IP so the transparent TCP proxy
// can bridge them (FJB-79). The orchestrator's proxy preserves TLS
// end-to-end with the real upstream — no LAN-CA injection needed.
type LocalDataOverride struct {
	// Name is the hostname (without trailing dot; unbound formats
	// elsewhere). Example: "git.stern.ca".
	Name string

	// AddrV4 is the IPv4 address unbound should answer for Name.
	// Typically the orchestrator's WG IP (e.g. "10.99.0.1").
	AddrV4 string
}

// RenderUnbound returns the /etc/unbound/unbound.conf content for the
// cache nanode. The resolver binds the cache's VPC IP, answers the
// short name "cache" with that same IP (so workers can `docker pull
// cache:5000/...`), forwards DNSForwardZones to LANNameserver over the
// IPsec tunnel, and defers everything else to public resolvers.
//
// Inputs.LocalOverrides (when set) emits additional authoritative
// local-data rows — used under the WG path to rewrite specific LAN
// hostnames to the orchestrator's WG IP.
//
// Consumed fields: CacheVPCIP, WorkerVPCSubnet, LANNameserver,
// DNSForwardZones, LocalOverrides.
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

	// Authoritative overrides for LAN hostnames under the WG path:
	// each maps a LAN service to the orchestrator's WG IP so the
	// transparent proxy can bridge it (FJB-79).
	for _, ov := range in.LocalOverrides {
		fmt.Fprintf(&b, "  # FJB-54: rewrite %s → orchestrator WG IP (transparent proxy bridges to real upstream).\n", ov.Name)
		fmt.Fprintf(&b, "  local-zone: %q static\n", ov.Name+".")
		fmt.Fprintf(&b, "  local-data: %q\n\n", ov.Name+". IN A "+ov.AddrV4)
	}

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
