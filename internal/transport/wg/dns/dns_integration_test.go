//go:build wgintegration

package dns_test

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	wgdns "github.com/hstern/fj-bellows/internal/transport/wg/dns"
)

// netstackListener adapts a *netstack.Net into the dns.Listener
// interface. Only built under the wgintegration build tag — the
// production wiring will use a similar adapter from the orchestrator
// (FJB-90).
type netstackListener struct {
	tnet *netstack.Net
}

func (l *netstackListener) ListenPacket(network, address string) (net.PacketConn, error) {
	if network != "udp" && network != "udp4" && network != "udp6" {
		return nil, fmt.Errorf("unsupported network: %s", network)
	}
	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	return l.tnet.ListenUDP(udpAddr)
}

func (l *netstackListener) Listen(network, address string) (net.Listener, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("unsupported network: %s", network)
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, err
	}
	return l.tnet.ListenTCP(tcpAddr)
}

// TestResponderInNetstack brings up two wireguard-go netstacks talking
// to each other, runs the DNS responder on side A, and queries it from
// side B's netstack. Validates the full stack: gVisor UDP/TCP, the WG
// tunnel, and the responder's bind path.
func TestResponderInNetstack(t *testing.T) {
	portA := freeUDPPort(t)
	portB := freeUDPPort(t)

	privA, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	privB, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	addrA := netip.MustParseAddr("10.99.0.1")
	addrB := netip.MustParseAddr("10.99.0.2")

	tnetA, cleanupA := newNetstack(t, privA, addrA, portA, privB.PublicKey(), portB, addrB)
	defer cleanupA()
	tnetB, cleanupB := newNetstack(t, privB, addrB, portB, privA.PublicKey(), portA, addrA)
	defer cleanupB()

	cacheIP := netip.MustParseAddr("192.168.42.7")
	r, err := wgdns.New(wgdns.Config{
		ListenAddr: net.JoinHostPort(addrA.String(), "53"),
		Listener:   &netstackListener{tnet: tnetA},
		Table:      wgdns.MapTable{"cache": cacheIP},
		Host:       failingResolver{},
	})
	if err != nil {
		t.Fatalf("New responder: %v", err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start responder: %v", err)
	}
	defer r.Close()

	// Query from B's netstack. Wait for the WG handshake to settle —
	// the first attempt may race the keepalive tick.
	query := buildQuery(t, 0xcafe, "cache.", dnsmessage.TypeA)

	deadline := time.Now().Add(10 * time.Second)
	var resp []byte
	for time.Now().Before(deadline) {
		c, err := tnetB.DialUDPAddrPort(netip.AddrPort{}, netip.AddrPortFrom(addrA, 53))
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write(query); err != nil {
			c.Close()
			time.Sleep(200 * time.Millisecond)
			continue
		}
		buf := make([]byte, 1500)
		n, err := c.Read(buf)
		c.Close()
		if err == nil {
			resp = buf[:n]
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if resp == nil {
		t.Fatal("no response from responder through netstack")
	}

	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if h.ID != 0xcafe {
		t.Errorf("ID: got %x want cafe", h.ID)
	}
	if h.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("RCode: got %v", h.RCode)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatal(err)
	}
	rh, err := p.AnswerHeader()
	if err != nil {
		t.Fatalf("answer header: %v", err)
	}
	if rh.Type != dnsmessage.TypeA {
		t.Errorf("rtype: got %v", rh.Type)
	}
	a, err := p.AResource()
	if err != nil {
		t.Fatal(err)
	}
	got := netip.AddrFrom4(a.A)
	if got != cacheIP {
		t.Errorf("addr: got %v want %v", got, cacheIP)
	}
}

type failingResolver struct{}

func (failingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, fmt.Errorf("not allowed in integration test")
}

func buildQuery(t *testing.T, id uint16, name string, qtype dnsmessage.Type) []byte {
	t.Helper()
	n, err := dnsmessage.NewName(name)
	if err != nil {
		t.Fatal(err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: n, Type: qtype, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func newNetstack(
	t *testing.T,
	priv wgtypes.Key,
	local netip.Addr,
	listenPort int,
	peerPub wgtypes.Key,
	peerPort int,
	peerAddr netip.Addr,
) (*netstack.Net, func()) {
	t.Helper()
	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{local}, nil, 1280)
	if err != nil {
		t.Fatal(err)
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), &device.Logger{
		Verbosef: func(string, ...any) {},
		Errorf:   func(string, ...any) {},
	})
	cfg := fmt.Sprintf(
		"private_key=%x\nlisten_port=%d\npublic_key=%x\nendpoint=127.0.0.1:%d\npersistent_keepalive_interval=1\nallowed_ip=%s/32\n",
		priv[:], listenPort, peerPub[:], peerPort, peerAddr,
	)
	if err := dev.IpcSet(cfg); err != nil {
		dev.Close()
		t.Fatal(err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		t.Fatal(err)
	}
	return tnet, func() { dev.Close() }
}
