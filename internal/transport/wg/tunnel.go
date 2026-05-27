package wg

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Config is the in-process WireGuard tunnel's configuration. Mirrors a
// subset of config.Transport.WG (kept distinct so this package doesn't
// take a config dep — callers translate at boundary).
type Config struct {
	// PrivateKey is the orchestrator's Curve25519 private key.
	// Load-or-generate via LoadOrGenerateKey before constructing.
	PrivateKey wgtypes.Key

	// LocalAddr is the tunnel-side IPv4 address the netstack interface
	// binds to (e.g. "10.99.0.1"). Workers reach this address through
	// the cache nanode's routing; the transparent-proxy listeners bind
	// on it.
	LocalAddr netip.Addr

	// ListenPort is the local UDP listen port for the WG bind. 0 picks
	// an ephemeral port — fine for the orchestrator (NAT-traversing
	// initiator) since the cache learns the source from the wire.
	// Tests use 0 + retrieve the port via Device.
	ListenPort int

	// MTU defaults to DefaultMTU when zero.
	MTU int

	// KeepaliveInterval applied to the peer (PersistentKeepalive).
	// Zero defaults to DefaultKeepaliveInterval (1s) — pins NAT mapping
	// warm for the orchestrator side of the tunnel.
	KeepaliveInterval time.Duration

	// Peer is the single WireGuard peer (cache nanode in fjb's
	// architecture; only point-to-point tunnels supported).
	Peer Peer

	// Logger receives WG-internal events (handshake, peer up/down).
	// Falls back to slog.Default when nil.
	Logger *slog.Logger
}

// Peer describes a single WireGuard peer.
type Peer struct {
	// PublicKey is the peer's Curve25519 public key.
	PublicKey wgtypes.Key

	// Endpoint is the peer's UDP address ("host:port"). For the cache
	// nanode this is the public IP + WG port; for tests, it's the
	// other tunnel's loopback bind.
	Endpoint *net.UDPAddr

	// AllowedIPs are the CIDRs that route through this peer. From the
	// orchestrator's side, includes the cache's WG IP and (typically)
	// the worker VPC subnet — the dispatcher dials VPC IPs through the
	// same tunnel (FJB-64).
	AllowedIPs []netip.Prefix
}

// Tunnel is the in-process WireGuard endpoint. Owns a wireguard-go
// Device backed by gVisor's netstack — no host TUN, no kernel-level
// routing or firewall. Exposes tunnel-side Listen / Dial via the
// embedded netstack net.
type Tunnel struct {
	dev    *device.Device
	tnet   *netstack.Net
	pubkey wgtypes.Key
	log    *slog.Logger

	mu     sync.Mutex
	closed bool
}

// New constructs and brings up a Tunnel. Returns once the device is
// configured and Up; the peer handshake is asynchronous (happens on
// first traffic or first PersistentKeepalive tick).
func New(cfg Config) (*Tunnel, error) {
	if cfg.PrivateKey == (wgtypes.Key{}) {
		return nil, ErrPrivateKeyRequired
	}
	if !cfg.LocalAddr.IsValid() {
		return nil, errors.New("wg: LocalAddr is required")
	}
	if cfg.Peer.PublicKey == (wgtypes.Key{}) {
		return nil, errors.New("wg: Peer.PublicKey is required")
	}
	if cfg.Peer.Endpoint == nil {
		return nil, ErrPeerEndpointRequired
	}

	mtu := cfg.MTU
	if mtu == 0 {
		mtu = DefaultMTU
	}
	keepalive := cfg.KeepaliveInterval
	if keepalive == 0 {
		keepalive = DefaultKeepaliveInterval
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	tun, tnet, err := netstack.CreateNetTUN(
		[]netip.Addr{cfg.LocalAddr},
		nil, // DNS via the tunnel — workers point at cache unbound directly; not needed here
		mtu,
	)
	if err != nil {
		return nil, fmt.Errorf("wg: create netstack tun: %w", err)
	}

	// wireguard-go's logger is a struct of two printf-style functions.
	// Surface its output through our slog at Debug level — useful when
	// debugging handshake failures, otherwise quiet.
	wgLog := &device.Logger{
		Verbosef: func(format string, args ...any) {
			log.Debug("wg/verbose", "msg", fmt.Sprintf(format, args...))
		},
		Errorf: func(format string, args ...any) {
			log.Warn("wg/error", "msg", fmt.Sprintf(format, args...))
		},
	}

	dev := device.NewDevice(tun, conn.NewDefaultBind(), wgLog)
	uapi := buildUAPI(cfg.PrivateKey, cfg.ListenPort, cfg.Peer, keepalive)
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wg: device IpcSet: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wg: device Up: %w", err)
	}

	pub := cfg.PrivateKey.PublicKey()
	log.Info("wg tunnel up",
		"local_addr", cfg.LocalAddr.String(),
		"public_key", pub.String(),
		"peer_endpoint", cfg.Peer.Endpoint.String(),
		"keepalive", keepalive.String(),
	)

	return &Tunnel{
		dev:    dev,
		tnet:   tnet,
		pubkey: pub,
		log:    log,
	}, nil
}

// PublicKey returns the tunnel's own WireGuard public key. The
// orchestrator logs this on first start so the operator can paste it
// into the cache nanode's wg-quick config.
func (t *Tunnel) PublicKey() wgtypes.Key {
	return t.pubkey
}

// ListenTCP returns a net.Listener accepting TCP connections on the
// tunnel-side address. The TCPAddr's IP must equal Config.LocalAddr;
// passing other addresses is undefined.
func (t *Tunnel) ListenTCP(addr *net.TCPAddr) (net.Listener, error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, errors.New("wg: tunnel is closed")
	}
	return t.tnet.ListenTCP(addr)
}

// DialContext dials a TCP connection through the tunnel. Used by the
// orchestrator's dispatcher to reach worker VPC IPs (FJB-64) via the
// cache nanode's routing.
func (t *Tunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, errors.New("wg: tunnel is closed")
	}
	return t.tnet.DialContext(ctx, network, address)
}

// Close stops the device and releases resources. Idempotent.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	t.dev.Close()
	return nil
}

// buildUAPI assembles the wireguard-go IPC configuration string. Keys
// are hex-encoded per the UAPI protocol (distinct from the base64 form
// wg-tools uses). Allowed-IPs and the endpoint go into the same blob.
func buildUAPI(priv wgtypes.Key, listenPort int, peer Peer, keepalive time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", hex.EncodeToString(priv[:]))
	if listenPort != 0 {
		fmt.Fprintf(&b, "listen_port=%d\n", listenPort)
	}
	fmt.Fprintf(&b, "public_key=%s\n", hex.EncodeToString(peer.PublicKey[:]))
	fmt.Fprintf(&b, "endpoint=%s\n", peer.Endpoint.String())
	fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", int(keepalive.Seconds()))
	for _, prefix := range peer.AllowedIPs {
		fmt.Fprintf(&b, "allowed_ip=%s\n", prefix.String())
	}
	return b.String()
}
