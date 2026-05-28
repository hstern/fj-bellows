package cachegateway

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// WGInputs feeds RenderWGQuick. Kept separate from Inputs because the
// iptables renderer and the wg-quick renderer consume disjoint subsets
// of the cache-side state — keeping the two struct shapes distinct
// makes the dependencies obvious.
type WGInputs struct {
	// CachePrivateKey is the cache nanode's WG private key (Curve25519,
	// base64). Generated + persisted on the cache; the operator pastes
	// the matching public key into the orchestrator's transport.wg.peer
	// block.
	CachePrivateKey string

	// CacheWGAddr is the cache nanode's tunnel-side address — a single
	// host inside the overlay prefix (e.g. 100.64.0.2 when the overlay
	// is 100.64.0.0/30). Rendered as Address = <addr>/32.
	CacheWGAddr netip.Addr

	// ListenPort is the UDP port the cache's WG interface listens on.
	// Inbound from anywhere — WG drops unauthenticated packets, so
	// wide-open is safe and matches the Linode firewall rule fjb
	// applies.
	ListenPort int

	// OrchestratorPublicKey is the orchestrator's WG public key
	// (Curve25519, base64).
	OrchestratorPublicKey string

	// AllowedIPs is the union of destination prefixes routed through
	// the orchestrator peer from the cache side. Computed by the caller
	// from the ACL registry's snapshot (orchestrator /32 + each
	// ACL-resolved CIDR/IP). One comma-separated AllowedIPs line is
	// emitted in the [Peer] block.
	//
	// Must contain at least one prefix (the orchestrator /32 is the
	// minimum useful set).
	AllowedIPs []netip.Prefix
}

func (i WGInputs) validate() error {
	if i.CachePrivateKey == "" {
		return errors.New("cachegateway: CachePrivateKey is required")
	}
	if !i.CacheWGAddr.IsValid() {
		return errors.New("cachegateway: CacheWGAddr is required")
	}
	if i.ListenPort < 1 || i.ListenPort > 65535 {
		return fmt.Errorf("cachegateway: ListenPort %d out of range", i.ListenPort)
	}
	if i.OrchestratorPublicKey == "" {
		return errors.New("cachegateway: OrchestratorPublicKey is required")
	}
	if len(i.AllowedIPs) == 0 {
		return errors.New("cachegateway: AllowedIPs must list at least one prefix")
	}
	for _, p := range i.AllowedIPs {
		if !p.IsValid() {
			return errors.New("cachegateway: AllowedIPs contains an invalid prefix")
		}
	}
	return nil
}

// RenderWGQuick returns the /etc/wireguard/wg0.conf content for the
// cache nanode (kernel WireGuard via wg-quick). The orchestrator is
// configured WITHOUT an Endpoint — it initiates from behind NAT and
// the cache learns the source IP:port from the wire. PersistentKeepalive
// is also unset on this side; only the initiator (orchestrator) keeps
// the NAT mapping warm.
//
// Operator workflow:
//
//  1. Generate cache private key: `wg genkey | tee /etc/wireguard/private.key`.
//  2. Render this config into /etc/wireguard/wg0.conf (chmod 600).
//  3. `systemctl enable --now wg-quick@wg0`.
//  4. Capture `wg show wg0 public-key` and paste it into the
//     orchestrator's transport.wg.peer.public_key.
//  5. Capture the orchestrator's first-start logged public key and
//     paste it into this template's OrchestratorPublicKey input.
//  6. Re-render + reload (`systemctl restart wg-quick@wg0`).
//
// Key exchange is the one human-coupled step; everything else is
// declarative + idempotent.
func RenderWGQuick(in WGInputs) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# wg0.conf — fj-bellows cache nanode (FJB-54, FJB-78)\n")
	b.WriteString("# Generated; do not edit by hand.\n")
	b.WriteString("#\n")
	b.WriteString("# This is the kernel-WireGuard config for the cache side of the\n")
	b.WriteString("# point-to-point tunnel to the orchestrator. The orchestrator is\n")
	b.WriteString("# the initiator (NAT-traversing); we only listen + learn its\n")
	b.WriteString("# endpoint from the first handshake.\n\n")
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "Address = %s/%d\n", in.CacheWGAddr, hostBits(in.CacheWGAddr))
	fmt.Fprintf(&b, "ListenPort = %d\n", in.ListenPort)
	fmt.Fprintf(&b, "PrivateKey = %s\n\n", in.CachePrivateKey)
	b.WriteString("[Peer]\n")
	b.WriteString("# Orchestrator (initiator, behind operator-side NAT).\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", in.OrchestratorPublicKey)
	fmt.Fprintf(&b, "AllowedIPs = %s\n", joinPrefixes(in.AllowedIPs))
	b.WriteString("# Endpoint intentionally omitted — orchestrator dials us first,\n")
	b.WriteString("# we learn its source IP:port from the wire.\n")
	b.WriteString("# PersistentKeepalive intentionally omitted — only the orchestrator\n")
	b.WriteString("# side runs keepalive (it's the side traversing NAT).\n")
	return b.String(), nil
}

// hostBits returns the host-route prefix length for the address family
// (32 for v4, 128 for v6).
func hostBits(a netip.Addr) int {
	if a.Is6() && !a.Is4In6() {
		return 128
	}
	return 32
}

// joinPrefixes renders a netip.Prefix slice in wg-quick's expected
// AllowedIPs shape: comma-separated, single space after each comma.
func joinPrefixes(prefixes []netip.Prefix) string {
	parts := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, ", ")
}
