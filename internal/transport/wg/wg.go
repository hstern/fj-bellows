// Package wg embeds WireGuard inside the orchestrator process via
// wireguard-go's userspace netstack mode (FJB-54, cache-as-gateway).
//
// The transport is a single point-to-point tunnel to the cache nanode.
// The orchestrator is the initiator (acts behind operator-side NAT),
// the cache is the listener (has a public IP). PersistentKeepalive
// keeps the NAT mapping warm — see Config.KeepaliveInterval.
//
// The package exposes tunnel-side Listen / Dial via netstack so the
// orchestrator can run transparent TCP proxies entirely in-process,
// with no host TUN device and no host-level routing changes. Same
// model as Tailscale's tsnet.
//
// Workers don't run WG — they reach LAN destinations through the cache
// nanode (kernel WG), and the cache's iptables FORWARD chain carries
// their traffic into the wg0 interface that terminates on the
// orchestrator's netstack instance.
package wg

import (
	"errors"
	"time"
)

// DefaultKeepaliveInterval is the WireGuard persistent-keepalive delay
// applied when Config.KeepaliveInterval is zero. 1 second is more
// aggressive than WireGuard's own 25s default — it pins the
// NAT mapping continuously warm and keeps first-request latency
// sub-second after long quiet periods.
//
// Bandwidth cost: a keepalive is one 60-byte packet at the IPv4 layer.
// At 1 Hz over a month: ~150 MB. Trivial against any reasonable plan;
// operators on metered links can bump to 25s (WireGuard's default).
const DefaultKeepaliveInterval = 1 * time.Second

// DefaultMTU is the WireGuard tunnel MTU. 1280 leaves headroom for IPv6
// + WireGuard's own 60-byte transport-header overhead under most paths
// without fragmentation. Matches Tailscale's default.
const DefaultMTU = 1280

// ErrPrivateKeyRequired is returned by New when Config.PrivateKey is
// the zero value. Callers should load-or-generate via the key helpers
// in this package first.
var ErrPrivateKeyRequired = errors.New("wg: private key is required")

// ErrPeerEndpointRequired is returned by New when the configured peer
// has no endpoint. Cache nanode endpoint is well-known (operator
// config); workers don't run WG so peer setup is always cache-only.
var ErrPeerEndpointRequired = errors.New("wg: peer endpoint is required")
