package cachegateway

import (
	"fmt"
	"strings"
)

// RenderStrongSwanResponder returns the /etc/ipsec.conf content for the
// cache nanode (the IKEv2 responder). It also returns the matching
// /etc/ipsec.secrets line.
//
// Consumed fields: CachePublicIP, LANGatewayPublicIP, WorkerVPCSubnet,
// Tunnel.Routes, PSK.
func RenderStrongSwanResponder(in Inputs) (ipsecConf, ipsecSecrets string, err error) {
	if err := in.validate(); err != nil {
		return "", "", err
	}
	var b strings.Builder
	b.WriteString("# ipsec.conf — fj-bellows cache nanode (FJB-71, IKEv2 responder)\n")
	b.WriteString("# Generated; do not edit by hand.\n\n")
	b.WriteString("config setup\n")
	b.WriteString("  charondebug=\"ike 1, knl 1, cfg 0\"\n")
	b.WriteString("  uniqueids=yes\n\n")
	b.WriteString("conn fjb-cache-gateway\n")
	b.WriteString("  auto=add\n")
	b.WriteString("  keyexchange=ikev2\n")
	fmt.Fprintf(&b, "  left=%s\n", in.CachePublicIP)
	fmt.Fprintf(&b, "  leftsubnet=%s\n", in.WorkerVPCSubnet)
	b.WriteString("  leftid=@fjb-cache\n")
	fmt.Fprintf(&b, "  right=%s\n", in.LANGatewayPublicIP)
	fmt.Fprintf(&b, "  rightsubnet=%s\n", joinCIDRs(in.Tunnel.Routes))
	b.WriteString("  rightid=@fjb-lan\n")
	b.WriteString("  authby=secret\n")
	b.WriteString("  ike=aes256-sha256-modp2048\n")
	b.WriteString("  esp=aes256-sha256\n")
	b.WriteString("  ikelifetime=8h\n")
	b.WriteString("  lifetime=1h\n")
	b.WriteString("  dpdaction=restart\n")
	b.WriteString("  dpddelay=30s\n")
	b.WriteString("  dpdtimeout=120s\n")
	b.WriteString("  closeaction=restart\n")

	secrets := fmt.Sprintf("# ipsec.secrets — fj-bellows cache nanode (FJB-71)\n@fjb-cache @fjb-lan : PSK %q\n", in.PSK)
	return b.String(), secrets, nil
}

// RenderStrongSwanInitiator returns the /etc/ipsec.conf content for the
// LAN-side host (the IKEv2 initiator) + the matching ipsec.secrets.
//
// Consumed fields: CachePublicIP, LANGatewayPublicIP, WorkerVPCSubnet,
// Tunnel.Routes, PSK.
func RenderStrongSwanInitiator(in Inputs) (ipsecConf, ipsecSecrets string, err error) {
	if err := in.validate(); err != nil {
		return "", "", err
	}
	var b strings.Builder
	b.WriteString("# ipsec.conf — fj-bellows LAN-side host (FJB-73, IKEv2 initiator)\n")
	b.WriteString("# Generated; do not edit by hand.\n\n")
	b.WriteString("config setup\n")
	b.WriteString("  charondebug=\"ike 1, knl 1, cfg 0\"\n")
	b.WriteString("  uniqueids=yes\n\n")
	b.WriteString("conn fjb-cache-gateway\n")
	b.WriteString("  auto=start\n")
	b.WriteString("  keyexchange=ikev2\n")
	fmt.Fprintf(&b, "  left=%s\n", in.LANGatewayPublicIP)
	fmt.Fprintf(&b, "  leftsubnet=%s\n", joinCIDRs(in.Tunnel.Routes))
	b.WriteString("  leftid=@fjb-lan\n")
	fmt.Fprintf(&b, "  right=%s\n", in.CachePublicIP)
	fmt.Fprintf(&b, "  rightsubnet=%s\n", in.WorkerVPCSubnet)
	b.WriteString("  rightid=@fjb-cache\n")
	b.WriteString("  authby=secret\n")
	b.WriteString("  ike=aes256-sha256-modp2048\n")
	b.WriteString("  esp=aes256-sha256\n")
	b.WriteString("  ikelifetime=8h\n")
	b.WriteString("  lifetime=1h\n")
	b.WriteString("  dpdaction=restart\n")
	b.WriteString("  dpddelay=30s\n")
	b.WriteString("  dpdtimeout=120s\n")
	b.WriteString("  closeaction=restart\n")

	secrets := fmt.Sprintf("# ipsec.secrets — fj-bellows LAN-side host (FJB-73)\n@fjb-lan @fjb-cache : PSK %q\n", in.PSK)
	return b.String(), secrets, nil
}

// joinCIDRs renders []string{"a/8","b/16"} as "a/8,b/16" — strongSwan's
// expected subnet list format.
func joinCIDRs(cidrs []string) string {
	return strings.Join(cidrs, ",")
}
