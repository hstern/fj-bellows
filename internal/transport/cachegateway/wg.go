package cachegateway

import (
	"errors"
	"fmt"
	"strings"
)

// WGInputs feeds RenderWGQuick. Kept separate from Inputs because the
// WG path doesn't share many fields with the IPsec path — keeping the
// two struct shapes distinct makes the dependencies obvious and
// avoids "is this field set?" surprises during the pivot.
type WGInputs struct {
	// CachePrivateKey is the cache nanode's WG private key (Curve25519,
	// base64). Generated + persisted on the cache; the operator pastes
	// the matching public key into the orchestrator's transport.wg.peer
	// block.
	CachePrivateKey string

	// CacheWGAddr is the cache nanode's tunnel-side IPv4 + prefix
	// (e.g. "10.99.0.2/32"). One /32 host on the WG mesh.
	CacheWGAddr string

	// ListenPort is the UDP port the cache's WG interface listens on.
	// Inbound from anywhere — WG drops unauthenticated packets, so
	// wide-open is safe and matches the Linode firewall rule fjb
	// applies (FJB-65, post-WG-pivot).
	ListenPort int

	// OrchestratorPublicKey is the orchestrator's WG public key
	// (Curve25519, base64). Operator pastes after running fj-bellows
	// once (the daemon generates on first start, logs the public
	// key, and waits for the peer to come up).
	OrchestratorPublicKey string

	// OrchestratorAllowedIPs is the list of CIDRs routed through the
	// orchestrator peer from the cache's side. Typically one /32 (the
	// orchestrator's WG IP); no LAN subnet — the orchestrator is the
	// transparent-proxy gateway, not a routing peer for LAN networks.
	OrchestratorAllowedIPs []string

	// WorkerVPCSubnet is the cache's view of the worker VPC (e.g.
	// "10.0.0.0/24"). Currently unused in the wg-quick render itself
	// (the iptables FORWARD chain owns worker-traffic gating, see
	// RenderCacheIPTables) — kept on the inputs struct for forward
	// compatibility with PostUp/PostDown one-liners that may want it.
	WorkerVPCSubnet string
}

func (i WGInputs) validate() error {
	if i.CachePrivateKey == "" {
		return errors.New("cachegateway: CachePrivateKey is required")
	}
	if i.CacheWGAddr == "" {
		return errors.New("cachegateway: CacheWGAddr is required")
	}
	if i.ListenPort < 1 || i.ListenPort > 65535 {
		return fmt.Errorf("cachegateway: ListenPort %d out of range", i.ListenPort)
	}
	if i.OrchestratorPublicKey == "" {
		return errors.New("cachegateway: OrchestratorPublicKey is required")
	}
	if len(i.OrchestratorAllowedIPs) == 0 {
		return errors.New("cachegateway: OrchestratorAllowedIPs must list at least one CIDR")
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
	fmt.Fprintf(&b, "Address = %s\n", in.CacheWGAddr)
	fmt.Fprintf(&b, "ListenPort = %d\n", in.ListenPort)
	fmt.Fprintf(&b, "PrivateKey = %s\n\n", in.CachePrivateKey)
	b.WriteString("[Peer]\n")
	b.WriteString("# Orchestrator (initiator, behind operator-side NAT).\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", in.OrchestratorPublicKey)
	fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(in.OrchestratorAllowedIPs, ", "))
	b.WriteString("# Endpoint intentionally omitted — orchestrator dials us first,\n")
	b.WriteString("# we learn its source IP:port from the wire.\n")
	b.WriteString("# PersistentKeepalive intentionally omitted — only the orchestrator\n")
	b.WriteString("# side runs keepalive (it's the side traversing NAT).\n")
	return b.String(), nil
}
