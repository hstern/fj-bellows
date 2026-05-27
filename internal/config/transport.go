package config

import (
	"fmt"
	"net"
	"strings"
)

// Transport configures the dispatch transport.
//
// Mode controls which transport architecture is active:
//
//   - "" / "ssh" (default): legacy SSH-on-public-IP dispatch. The
//     orchestrator dials each worker on its public IPv4 over SSH;
//     workers reach a LAN-only Forgejo via a reverse port-forward
//     carried on the dispatch SSH session.
//   - "cache-gateway": the cache-as-gateway architecture (FJB-54).
//     The cache nanode terminates an IPsec tunnel from the LAN side
//     and runs the DNS resolver + routing that lets workers reach
//     LAN destinations by name. Tunnel describes which CIDRs are
//     routed across the tunnel and which traffic is permitted to
//     emerge onto the LAN.
//
// The "ssh" default keeps existing deployments working unchanged
// until they opt in by switching Mode.
type Transport struct {
	Mode   string  `yaml:"mode"`
	Tunnel *Tunnel `yaml:"tunnel"`
}

// Tunnel is the IPsec + cache-as-gateway tunnel configuration.
// Required when Transport.Mode == "cache-gateway"; ignored otherwise.
type Tunnel struct {
	// Routes is the list of LAN-side CIDRs reachable from workers via the
	// tunnel. Worker cloud-init renders an explicit route for each via the
	// cache's VPC IP; everything not in the list takes the worker's default
	// gateway (provider NAT egress).
	Routes []string `yaml:"routes"`

	// LANEgress is the allow-list of (source, destination, port, proto)
	// tuples permitted to emerge from the tunnel onto the LAN. Two
	// enforcement points consume the same list: the cache nanode's
	// iptables FORWARD chain (so workers can only initiate matching
	// flows) and the LAN-side outbound firewall (last line of defence
	// against a compromised worker pivoting). Anything not listed is
	// denied.
	LANEgress []LANEgressRule `yaml:"lan_egress"`
}

// LANEgressRule is one entry in the tunnel egress allow-list.
type LANEgressRule struct {
	// From identifies the traffic source. Currently "worker-vpc" only,
	// meaning the configured worker VPC subnet on the cache side. The
	// renderer resolves this label to the concrete CIDR via the active
	// provider's VPC config. Future values might split workers into
	// sub-cohorts (per-tag etc.) without breaking the schema.
	From string `yaml:"from"`

	// To is the destination IP, single host only for now (CIDR support
	// is a future extension when the operator needs ranges).
	To string `yaml:"to"`

	// Port is the TCP/UDP port the rule applies to (1-65535).
	Port int `yaml:"port"`

	// Proto is "tcp" or "udp".
	Proto string `yaml:"proto"`
}

// Transport mode constants. Empty Mode also means TransportSSH (back-compat).
const (
	TransportSSH          = "ssh"
	TransportCacheGateway = "cache-gateway"
)

// Egress source labels. "worker-vpc" resolves to the configured worker VPC
// subnet via the active provider.
const EgressFromWorkerVPC = "worker-vpc"

func (t *Transport) applyDefaults() {
	if t.Mode == "" {
		t.Mode = TransportSSH
	}
}

func (t *Transport) validate() error {
	switch t.Mode {
	case TransportSSH:
		// Tunnel block is meaningless in SSH mode; we don't error if
		// present (operators may toggle modes mid-edit) but we ignore it.
		return nil
	case TransportCacheGateway:
		if t.Tunnel == nil {
			return fmt.Errorf("transport: mode %q requires a transport.tunnel block", t.Mode)
		}
		return t.Tunnel.validate()
	default:
		return fmt.Errorf("transport: unknown mode %q (want %q or %q)",
			t.Mode, TransportSSH, TransportCacheGateway)
	}
}

func (tn *Tunnel) validate() error {
	if len(tn.Routes) == 0 {
		return fmt.Errorf("transport.tunnel: routes must list at least one CIDR")
	}
	for i, r := range tn.Routes {
		if _, _, err := net.ParseCIDR(r); err != nil {
			return fmt.Errorf("transport.tunnel.routes[%d] = %q: %w", i, r, err)
		}
	}
	for i, rule := range tn.LANEgress {
		if err := rule.validate(); err != nil {
			return fmt.Errorf("transport.tunnel.lan_egress[%d]: %w", i, err)
		}
	}
	return nil
}

func (r *LANEgressRule) validate() error {
	switch r.From {
	case EgressFromWorkerVPC:
		// ok
	case "":
		return fmt.Errorf("from is required (e.g. %q)", EgressFromWorkerVPC)
	default:
		return fmt.Errorf("unknown from %q (want %q)", r.From, EgressFromWorkerVPC)
	}
	if r.To == "" {
		return fmt.Errorf("to is required")
	}
	if ip := net.ParseIP(r.To); ip == nil {
		return fmt.Errorf("to %q is not a valid IP address", r.To)
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("port %d out of range (want 1-65535)", r.Port)
	}
	switch strings.ToLower(r.Proto) {
	case "tcp", "udp":
		// ok
	case "":
		return fmt.Errorf("proto is required (\"tcp\" or \"udp\")")
	default:
		return fmt.Errorf("proto %q must be \"tcp\" or \"udp\"", r.Proto)
	}
	return nil
}
