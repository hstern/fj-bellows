// Package cachegateway renders the operator-facing config files that
// implement the cache-as-gateway transport architecture (FJB-54). It is
// the bridge between the in-fjb runtime state (the ACL registry's
// current address-set snapshot, plus a handful of network parameters)
// and the byte-level config that wg-quick and iptables expect.
//
// Under the ACL-driven WireGuard transport (post FJB-54 pivot), the
// cache nanode is a thin layer-3 forwarder: it terminates the WG tunnel
// to the orchestrator and forwards worker-VPC traffic ONLY to the
// AllowedIPs union derived from the ACL registry. There is no unbound,
// no strongSwan, and no LAN-side nftables — workers reach LAN services
// via the orchestrator's transparent TCP proxy, gated by ACL.
//
// The renderers are pure functions of an Inputs struct: same input,
// same output, golden-file testable. fjb does not apply the rendered
// configs to live hosts itself — that is an operator step today. A
// future revision may push them via SSH or a configuration-management
// agent on the cache nanode.
//
// Output shapes:
//
//   - RenderWGQuick        — /etc/wireguard/wg0.conf for the cache
//   - RenderCacheIPTables  — shell script that installs the FORWARD
//     rules + ip_forward sysctl on the cache nanode
package cachegateway

import (
	"errors"
	"net"
	"net/netip"
)

// Inputs feeds RenderCacheIPTables. Each renderer documents the subset
// of fields it actually consumes.
//
// The caller (orchestrator wiring; see FJB-90) is responsible for
// turning the ACL registry's address snapshot into AllowedIPs — this
// package only renders the result.
type Inputs struct {
	// CacheVPCIP is the cache nanode's IPv4 on the worker VPC. Currently
	// informational on the renderer side (reserved for future PostUp
	// hooks); kept in the struct so callers don't have to thread it in
	// later.
	CacheVPCIP string

	// WorkerVPCSubnet is the worker VPC CIDR (provider-supplied; e.g.
	// "10.0.0.0/24"). Used as the iptables FORWARD source filter — only
	// traffic originating in the worker VPC is eligible for forwarding
	// out wg0.
	WorkerVPCSubnet string

	// AllowedIPs is the ACL-derived union of destination prefixes the
	// cache will forward worker-VPC traffic to over wg0. The caller
	// computes this from the ACL registry's current snapshot (FJB-82 ACL
	// parser + FJB-90 orchestrator composition). One iptables ACCEPT
	// rule is emitted per prefix.
	//
	// The orchestrator's WG /32 is included here too — it's the
	// transparent-proxy hop for ACL'd destinations that the cache itself
	// cannot reach directly.
	AllowedIPs []netip.Prefix

	// OverlayPrefix is the WG overlay CIDR the cache + orchestrator
	// share (e.g. 100.64.0.0/30). Currently informational on the
	// renderer side; reserved for future PostUp hooks that may want to
	// reference it explicitly.
	OverlayPrefix netip.Prefix

	// OrchestratorWGAddr is the orchestrator's address on the WG
	// overlay (a single host in OverlayPrefix). Currently informational;
	// callers include it in AllowedIPs when composing the snapshot.
	OrchestratorWGAddr netip.Addr

	// CacheWGAddr is the cache's own address on the WG overlay (a
	// single host in OverlayPrefix). Currently informational on the
	// iptables side; RenderWGQuick consumes it via WGInputs.
	CacheWGAddr netip.Addr
}

// validate runs the basic sanity checks for the iptables renderer.
//
// CacheVPCIP is intentionally optional: the renderer doesn't emit it
// into the script body (it's reserved for future PostUp hooks). Cache
// cloud-init bake-in (FJB-98) needs to render the script at cache
// create time, before any VPC IP has been assigned — relaxing the
// requirement makes that flow possible without inventing a placeholder.
// When supplied it still has to be a valid IPv4/IPv6 literal.
func (i Inputs) validate() error {
	if i.CacheVPCIP != "" && net.ParseIP(i.CacheVPCIP) == nil {
		return errors.New("cachegateway: CacheVPCIP is not a valid IP")
	}
	if i.WorkerVPCSubnet == "" {
		return errors.New("cachegateway: WorkerVPCSubnet is required")
	}
	if _, _, err := net.ParseCIDR(i.WorkerVPCSubnet); err != nil {
		return errors.New("cachegateway: WorkerVPCSubnet is not a valid CIDR")
	}
	for _, p := range i.AllowedIPs {
		if !p.IsValid() {
			return errors.New("cachegateway: AllowedIPs contains an invalid prefix")
		}
	}
	return nil
}
