package config

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// DefaultWGKeepaliveInterval is the default persistent-keepalive delay
// for the WireGuard tunnel — more aggressive than WireGuard's 25s. The
// orchestrator is the NAT-traversing initiator, so the mapping must
// stay continuously warm; 1s pins it tight at ~150 MB/month, trivial
// against the cache nanode's 1 TB included transfer. Operators on
// metered links override via transport.wg.keepalive_interval.
const DefaultWGKeepaliveInterval = 1 * time.Second

// DefaultWGListenPort is the WireGuard project's de-facto default port
// — what operator muscle-memory expects, what most Linode firewall
// examples reference.
const DefaultWGListenPort = 51820

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

	// WG configures the embedded WireGuard tunnel that carries
	// worker-side traffic destined for the orchestrator's transparent
	// proxy listeners (FJB-78). Required when Mode == "cache-gateway";
	// ignored otherwise.
	WG *WG `yaml:"wg"`
}

// WG configures the orchestrator's embedded WireGuard tunnel
// (golang.zx2c4.com/wireguard in netstack mode). The orchestrator is
// always the initiator (operator-side NAT); the cache nanode is the
// public-IP listener.
type WG struct {
	// PrivateKeyFile is the path to the orchestrator's WG private key
	// (Curve25519, base64). The daemon load-or-generates it on first
	// start (mode 0600); operators rotate by removing the file.
	PrivateKeyFile string `yaml:"private_key_file"`

	// LocalAddr is the orchestrator's tunnel-side IPv4 + prefix
	// (e.g. "10.99.0.1/32"). The transparent-proxy listeners bind on
	// this address; the cache nanode's wg-quick config lists it as
	// the peer's AllowedIPs entry.
	LocalAddr string `yaml:"local_addr"`

	// KeepaliveInterval is the PersistentKeepalive setting on the
	// orchestrator → cache peer. Empty = DefaultWGKeepaliveInterval
	// (1s); operators on metered links bump to 25s (WireGuard's own
	// default) for ~6 MB/month instead of ~150 MB/month, trading
	// first-request latency for bandwidth.
	KeepaliveInterval Duration `yaml:"keepalive_interval"`

	// Peer is the cache nanode WireGuard peer. There is exactly one;
	// workers don't run WG.
	Peer WGPeer `yaml:"peer"`

	// Proxies is the list of transparent TCP-proxy listeners. Each
	// pair binds Listen on the orchestrator's WG interface and dials
	// Upstream over the orchestrator host's normal network. Workers
	// dial what they think is a LAN service (Listen address) and the
	// proxy bridges bytes to the real upstream. TLS terminates
	// end-to-end with the real upstream — proxy doesn't touch TLS.
	Proxies []WGProxy `yaml:"proxies"`
}

// WGPeer describes the cache nanode's WG endpoint.
type WGPeer struct {
	// PublicKey is the cache nanode's WG public key (Curve25519,
	// base64). Operator pastes this after installing wireguard-tools
	// on the cache.
	PublicKey string `yaml:"public_key"`

	// Endpoint is the cache nanode's public address (host:port).
	// Host may be an IP or a DNS name resolvable from the
	// orchestrator's host network.
	Endpoint string `yaml:"endpoint"`

	// AllowedIPs is the list of CIDRs routed through this peer.
	// Includes at least the cache's WG IP (/32); typically also
	// includes the worker VPC subnet so dispatcher dial-by-VPC-IP
	// (FJB-64) routes through the same tunnel.
	AllowedIPs []string `yaml:"allowed_ips"`
}

// WGProxy is one transparent TCP-proxy listener.
type WGProxy struct {
	// Listen is the orchestrator-side address (must be on the WG
	// interface), e.g. "10.99.0.1:443".
	Listen string `yaml:"listen"`

	// Upstream is the real LAN destination (host:port). Dialed over
	// the orchestrator host's normal network.
	Upstream string `yaml:"upstream"`
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

// EgressFromWorkerVPC is the source label for LANEgressRule entries whose
// traffic originates from the worker VPC subnet. The renderer resolves
// "worker-vpc" to the configured worker VPC CIDR via the active provider.
const EgressFromWorkerVPC = "worker-vpc"

func (t *Transport) applyDefaults() {
	if t.Mode == "" {
		t.Mode = TransportSSH
	}
	if t.WG != nil && t.WG.KeepaliveInterval == 0 {
		t.WG.KeepaliveInterval = Duration(DefaultWGKeepaliveInterval)
	}
}

func (t *Transport) validate() error {
	switch t.Mode {
	case TransportSSH:
		// Tunnel + WG blocks meaningless in SSH mode; we don't error
		// if present (operators may toggle modes mid-edit) but we
		// ignore them.
		return nil
	case TransportCacheGateway:
		if t.Tunnel == nil {
			return fmt.Errorf("transport: mode %q requires a transport.tunnel block", t.Mode)
		}
		if err := t.Tunnel.validate(); err != nil {
			return err
		}
		if t.WG != nil {
			if err := t.WG.validate(); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("transport: unknown mode %q (want %q or %q)",
			t.Mode, TransportSSH, TransportCacheGateway)
	}
}

func (w *WG) validate() error {
	if w.PrivateKeyFile == "" {
		return errors.New("transport.wg: private_key_file is required")
	}
	if w.LocalAddr == "" {
		return errors.New("transport.wg: local_addr is required")
	}
	if _, _, err := net.ParseCIDR(w.LocalAddr); err != nil {
		return fmt.Errorf("transport.wg.local_addr = %q: %w", w.LocalAddr, err)
	}
	if w.KeepaliveInterval < 0 {
		return errors.New("transport.wg: keepalive_interval must be non-negative")
	}
	if err := w.Peer.validate(); err != nil {
		return fmt.Errorf("transport.wg.peer: %w", err)
	}
	for i, p := range w.Proxies {
		if err := p.validate(); err != nil {
			return fmt.Errorf("transport.wg.proxies[%d]: %w", i, err)
		}
	}
	return nil
}

func (p *WGPeer) validate() error {
	if p.PublicKey == "" {
		return errors.New("public_key is required")
	}
	if p.Endpoint == "" {
		return errors.New("endpoint is required")
	}
	if _, _, err := net.SplitHostPort(p.Endpoint); err != nil {
		return fmt.Errorf("endpoint %q is not host:port: %w", p.Endpoint, err)
	}
	if len(p.AllowedIPs) == 0 {
		return errors.New("allowed_ips must list at least one CIDR")
	}
	for i, cidr := range p.AllowedIPs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("allowed_ips[%d] = %q: %w", i, cidr, err)
		}
	}
	return nil
}

func (px *WGProxy) validate() error {
	if px.Listen == "" {
		return errors.New("listen is required")
	}
	if _, _, err := net.SplitHostPort(px.Listen); err != nil {
		return fmt.Errorf("listen %q is not host:port: %w", px.Listen, err)
	}
	if px.Upstream == "" {
		return errors.New("upstream is required")
	}
	if _, _, err := net.SplitHostPort(px.Upstream); err != nil {
		return fmt.Errorf("upstream %q is not host:port: %w", px.Upstream, err)
	}
	return nil
}

func (tn *Tunnel) validate() error {
	if len(tn.Routes) == 0 {
		return errors.New("transport.tunnel: routes must list at least one CIDR")
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
		return errors.New("to is required")
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
		return errors.New(`proto is required ("tcp" or "udp")`)
	default:
		return fmt.Errorf("proto %q must be \"tcp\" or \"udp\"", r.Proto)
	}
	return nil
}
