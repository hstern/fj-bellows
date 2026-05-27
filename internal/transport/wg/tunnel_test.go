package wg

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// validConfig builds a Config that can be used with New (modulo the
// peer endpoint, which most negative tests override). Caller may
// mutate fields to exercise validation branches.
func validConfig(t *testing.T) Config {
	t.Helper()
	priv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	peerPriv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		PrivateKey:        priv,
		LocalAddr:         netip.MustParseAddr("10.99.0.1"),
		ListenPort:        0, // ephemeral
		KeepaliveInterval: 1 * time.Second,
		Peer: Peer{
			PublicKey:  peerPriv.PublicKey(),
			Endpoint:   &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 51820},
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.99.0.2/32")},
		},
	}
}

// TestNew_RejectsZeroPrivateKey ensures the constructor catches the
// zero-key path that an uninitialised Config would yield.
func TestNew_RejectsZeroPrivateKey(t *testing.T) {
	cfg := validConfig(t)
	cfg.PrivateKey = wgtypes.Key{}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("want ErrPrivateKeyRequired, got nil")
	}
	if !errors.Is(err, ErrPrivateKeyRequired) {
		t.Errorf("err = %v, want ErrPrivateKeyRequired", err)
	}
}

func TestNew_RejectsZeroPeerPublicKey(t *testing.T) {
	cfg := validConfig(t)
	cfg.Peer.PublicKey = wgtypes.Key{}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "Peer.PublicKey") {
		t.Errorf("err = %v, want Peer.PublicKey complaint", err)
	}
}

func TestNew_RejectsMissingEndpoint(t *testing.T) {
	cfg := validConfig(t)
	cfg.Peer.Endpoint = nil
	_, err := New(cfg)
	if !errors.Is(err, ErrPeerEndpointRequired) {
		t.Errorf("err = %v, want ErrPeerEndpointRequired", err)
	}
}

func TestNew_RejectsInvalidLocalAddr(t *testing.T) {
	cfg := validConfig(t)
	cfg.LocalAddr = netip.Addr{} // zero value
	_, err := New(cfg)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "LocalAddr") {
		t.Errorf("err = %v, want LocalAddr complaint", err)
	}
}

// TestNew_BringsTunnelUp constructs a real tunnel (no peer reachable —
// the configured endpoint is dead loopback) and verifies the basic
// shape: PublicKey populated, ListenTCP returns a listener, Close
// stops cleanly.
func TestNew_BringsTunnelUp(t *testing.T) {
	tn, err := New(validConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = tn.Close() }()

	if tn.PublicKey() == (wgtypes.Key{}) {
		t.Error("PublicKey is zero")
	}
	l, err := tn.ListenTCP(&net.TCPAddr{IP: net.ParseIP("10.99.0.1"), Port: 8080})
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("listener Close: %v", err)
	}
}

// TestClose_Idempotent — multiple Close calls are safe; subsequent
// operations error rather than panic.
func TestClose_Idempotent(t *testing.T) {
	tn, err := New(validConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := tn.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := tn.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	_, err = tn.ListenTCP(&net.TCPAddr{IP: net.ParseIP("10.99.0.1"), Port: 80})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("ListenTCP after Close: err = %v, want 'closed'", err)
	}
}

// TestBuildUAPI_Shape pins the IPC string format against drift. The
// UAPI string is what wireguard-go's IpcSet consumes; getting field
// names wrong is a silent misconfiguration.
func TestBuildUAPI_Shape(t *testing.T) {
	priv, _ := wgtypes.GeneratePrivateKey()
	peerPriv, _ := wgtypes.GeneratePrivateKey()
	peer := Peer{
		PublicKey: peerPriv.PublicKey(),
		Endpoint:  &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 51820},
		AllowedIPs: []netip.Prefix{
			netip.MustParsePrefix("10.99.0.2/32"),
			netip.MustParsePrefix("10.0.0.0/24"),
		},
	}
	got := buildUAPI(priv, 5555, peer, 25*time.Second)
	wantSubstrings := []string{
		"private_key=",
		"listen_port=5555\n",
		"public_key=",
		"endpoint=10.0.0.1:51820\n",
		"persistent_keepalive_interval=25\n",
		"allowed_ip=10.99.0.2/32\n",
		"allowed_ip=10.0.0.0/24\n",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in UAPI:\n%s", sub, got)
		}
	}
}

// TestBuildUAPI_OmitsListenPortWhenZero — zero port means "let the
// kernel pick an ephemeral source," which the orchestrator uses (it's
// the initiator, doesn't need a fixed port). UAPI must NOT emit
// listen_port=0 because some wg implementations interpret 0 as
// disable.
func TestBuildUAPI_OmitsListenPortWhenZero(t *testing.T) {
	priv, _ := wgtypes.GeneratePrivateKey()
	peerPriv, _ := wgtypes.GeneratePrivateKey()
	peer := Peer{
		PublicKey:  peerPriv.PublicKey(),
		Endpoint:   &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 51820},
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.99.0.2/32")},
	}
	got := buildUAPI(priv, 0, peer, time.Second)
	if strings.Contains(got, "listen_port=") {
		t.Errorf("unexpected listen_port line when port=0:\n%s", got)
	}
}
