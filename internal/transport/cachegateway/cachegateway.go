// Package cachegateway renders the operator-facing config files that
// implement the cache-as-gateway transport architecture (FJB-54). It is
// the bridge between the in-fjb configuration schema
// (config.Transport.Tunnel, FJB-72) and the byte-level config that
// strongSwan, unbound, iptables, and nftables expect.
//
// The renderers are pure functions of an Inputs struct: same input,
// same output, golden-file testable. fjb does not apply the rendered
// configs to live hosts itself — that is an operator step today. A
// future revision may push them via SSH or a configuration-management
// agent on the cache nanode and the LAN-side host.
//
// Output shapes:
//
//   - RenderUnbound      — /etc/unbound/unbound.conf for the cache
//   - RenderStrongSwanResponder — /etc/ipsec.conf (cache side, IKEv2 responder)
//   - RenderCacheIPTables       — shell-script that installs the FORWARD
//     rules + ip_forward sysctl on the cache nanode
//   - RenderStrongSwanInitiator — /etc/ipsec.conf (LAN side, IKEv2 initiator)
//   - RenderLANNftables         — shell-script for the LAN-side nftables
//     outbound filter (FJB-73 lan_egress allow-list)
package cachegateway

import (
	"errors"
	"net"

	"github.com/hstern/fj-bellows/internal/config"
)

// Inputs feeds every renderer in this package. Each renderer documents
// the subset of fields it actually consumes.
type Inputs struct {
	// Tunnel is the operator-supplied transport.tunnel block (FJB-72).
	// Routes drives strongSwan's right-subnet (the responder side) and
	// the LAN-side right-subnet on the initiator. LANEgress drives the
	// LAN-side nftables filter.
	Tunnel config.Tunnel

	// CacheVPCIP is the cache nanode's IPv4 on the worker VPC. Used as
	// the unbound listen address and as the iptables MASQUERADE source
	// for tunnel-bound worker traffic.
	CacheVPCIP string

	// WorkerVPCSubnet is the worker VPC CIDR (provider-supplied; e.g.
	// "10.0.0.0/24"). Used as the iptables FORWARD source filter and as
	// strongSwan's left-subnet on the responder side.
	WorkerVPCSubnet string

	// LANNameserver is the IP of the operator's LAN DNS server (e.g.
	// "192.168.0.2"). unbound forwards DNSForwardZones to it.
	LANNameserver string

	// DNSForwardZones is the list of DNS suffixes unbound should
	// forward to LANNameserver (e.g. ["stern.ca"]). Anything not
	// matching takes the default public-resolver path.
	DNSForwardZones []string

	// LANGatewayPublicIP is the operator-side IPsec endpoint's public
	// address — strongSwan-on-cache uses it as `right=` to validate
	// the initiator's source IP. The LAN-side initiator uses it as
	// `left=` (its own external address).
	LANGatewayPublicIP string

	// CachePublicIP is the cache nanode's public IPv4 — the IPsec
	// responder's `left=` and the LAN-side initiator's `right=`.
	CachePublicIP string

	// PSK is the IKEv2 pre-shared key used at both ends. Suitable for
	// PoC; production deployments should move to cert auth (a future
	// renderer refinement).
	PSK string
}

// validate runs the basic sanity checks every renderer wants in place
// before it produces output. Each renderer also pulls only the fields
// it needs, but validating up-front lets the caller surface config
// problems early.
func (i Inputs) validate() error {
	if i.CacheVPCIP == "" {
		return errors.New("cachegateway: CacheVPCIP is required")
	}
	if net.ParseIP(i.CacheVPCIP) == nil {
		return errors.New("cachegateway: CacheVPCIP is not a valid IP")
	}
	if i.WorkerVPCSubnet == "" {
		return errors.New("cachegateway: WorkerVPCSubnet is required")
	}
	if _, _, err := net.ParseCIDR(i.WorkerVPCSubnet); err != nil {
		return errors.New("cachegateway: WorkerVPCSubnet is not a valid CIDR")
	}
	if i.CachePublicIP == "" {
		return errors.New("cachegateway: CachePublicIP is required")
	}
	if i.LANGatewayPublicIP == "" {
		return errors.New("cachegateway: LANGatewayPublicIP is required")
	}
	if i.PSK == "" {
		return errors.New("cachegateway: PSK is required (PoC; cert auth is future work)")
	}
	for _, r := range i.Tunnel.Routes {
		if _, _, err := net.ParseCIDR(r); err != nil {
			return errors.New("cachegateway: Tunnel.Routes contains an invalid CIDR")
		}
	}
	return nil
}
