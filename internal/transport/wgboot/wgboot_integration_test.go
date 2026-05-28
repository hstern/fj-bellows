//go:build wgintegration

package wgboot

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/hstern/fj-bellows/internal/config"
	"github.com/hstern/fj-bellows/internal/transport/wg"
)

// TestBoot_FullStackBringup spins the boot path against an in-process
// peer Tunnel acting as the "cache nanode". Validates that every
// component comes up under the integration-real wireguard-go stack and
// that Close tears everything down cleanly.
//
// Build-tag gated (wgintegration) so the slow per-tunnel handshake
// doesn't run on every default `go test ./...`. Run with:
//
//	go test -tags wgintegration ./internal/transport/wgboot/
func TestBoot_FullStackBringup(t *testing.T) {
	t.Parallel()

	// Spin a peer (cache) tunnel on loopback so the orchestrator's
	// Boot has a real WG endpoint to dial.
	cachePriv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	orchPriv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	cacheAddr := netip.MustParseAddr("100.64.0.2")
	orchAddr := netip.MustParseAddr("100.64.0.1")

	cachePort := freeUDPPort(t)
	orchPort := freeUDPPort(t)

	cacheTun, err := wg.New(wg.Config{
		PrivateKey:        cachePriv,
		LocalAddr:         cacheAddr,
		ListenPort:        cachePort,
		KeepaliveInterval: 500 * time.Millisecond,
		Peer: wg.Peer{
			PublicKey:  orchPriv.PublicKey(),
			Endpoint:   &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: orchPort},
			AllowedIPs: []netip.Prefix{netip.PrefixFrom(orchAddr, 32)},
		},
	})
	if err != nil {
		t.Fatalf("cache tunnel: %v", err)
	}
	defer cacheTun.Close()

	// Stage the orchestrator private key on disk so LoadOrGenerateKey
	// finds it (mode 0600 required).
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "wg.key")
	if err := os.WriteFile(keyPath, []byte(orchPriv.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	transport := config.Transport{
		Mode: config.TransportCacheGateway,
		Tunnel: &config.Tunnel{
			Routes: []string{"10.0.0.0/24"},
		},
		WG: &config.WG{
			PrivateKeyFile:    keyPath,
			ListenPort:        orchPort,
			LocalAddr:         orchAddr.String() + "/32",
			KeepaliveInterval: config.Duration(500 * time.Millisecond),
			OverlayPrefix:     "100.64.0.0/30",
			Peer: config.WGPeer{
				PublicKey:  cachePriv.PublicKey().String(),
				Endpoint:   net.JoinHostPort("127.0.0.1", itoa(cachePort)),
				AllowedIPs: []string{cacheAddr.String() + "/32"},
			},
			ACL: []string{"tcp://192.0.2.1:443"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sink ACLSource
	stack, err := Boot(ctx, Config{
		Transport:  transport,
		ForgejoURL: "https://git.example.com",
		ACLSink:    func(s ACLSource) { sink = s },
		Cache: CacheRenderInputs{
			CacheVPCIP:      "10.0.0.5",
			WorkerVPCSubnet: "10.0.0.0/24",
		},
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer stack.Close()

	// Smoke checks: every component is non-nil + the ACL sink was
	// invoked + the registry has the operator + implicit entries.
	if stack.Tunnel == nil {
		t.Error("Tunnel is nil")
	}
	if stack.Registry == nil {
		t.Error("Registry is nil")
	}
	if stack.Resolver == nil {
		t.Error("Resolver is nil")
	}
	if stack.Responder == nil {
		t.Error("Responder is nil")
	}
	if stack.UDP == nil {
		t.Error("UDP forwarder is nil")
	}
	if stack.ICMP == nil {
		t.Error("ICMP bridge is nil")
	}
	if stack.PublicKey == "" {
		t.Error("PublicKey is empty")
	}
	if sink == nil {
		t.Fatal("ACLSink was never called")
	}
	cidrs := sink.AllowedIPsCIDRs()
	if len(cidrs) == 0 {
		t.Error("sink returned 0 CIDRs; want at least the literal ACL entry")
	}
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b [16]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = digits[i%10]
		i /= 10
	}
	return string(b[pos:])
}
