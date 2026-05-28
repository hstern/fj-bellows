package orchestrator

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

const (
	testPubIP = "1.2.3.4"
	testVPCIP = "10.0.0.5"
)

// TestCacheGatewayDispatcher_DoesNotImplementHostKeyPinner is the
// load-bearing structural assertion: the orchestrator's host-key
// generation code paths gate on type-asserting the dispatcher to
// HostKeyPinner. CacheGatewayDispatcher must NOT satisfy that
// interface so cloud-init skips host-key injection and the
// orchestrator skips per-VM ed25519 generation under FJB-54.
func TestCacheGatewayDispatcher_DoesNotImplementHostKeyPinner(t *testing.T) {
	var d any = &CacheGatewayDispatcher{}
	if _, ok := d.(HostKeyPinner); ok {
		t.Fatal("CacheGatewayDispatcher MUST NOT implement HostKeyPinner — orchestrator host-key generation must auto-skip under cache-gateway transport")
	}
}

// TestSSHDispatcher_StillImplementsHostKeyPinner — pair to the above,
// makes sure we didn't accidentally break the legacy path.
func TestSSHDispatcher_StillImplementsHostKeyPinner(t *testing.T) {
	var d any = &SSHDispatcher{}
	if _, ok := d.(HostKeyPinner); !ok {
		t.Fatal("SSHDispatcher must continue to implement HostKeyPinner under legacy ssh transport")
	}
}

// TestCacheGatewayDispatcher_SatisfiesDispatcher — compile-time-ish
// check that the new type plugs into the Dispatcher interface.
func TestCacheGatewayDispatcher_SatisfiesDispatcher(t *testing.T) {
	var _ Dispatcher = (*CacheGatewayDispatcher)(nil)
	_ = t // present to make this a real test fn, not just a global var decl
}

// TestCacheGatewayDispatcher_UsesInjectedDialFn is the FJB-92 unit test:
// when the dispatcher is wired with a DialFn (the WG tunnel's netstack
// DialContext in production), dial() routes through it instead of the
// host kernel's net.Dialer. We don't run a full SSH handshake — that's
// covered by the integration tests; we only need to confirm the
// DialFn was invoked with the expected target.
func TestCacheGatewayDispatcher_UsesInjectedDialFn(t *testing.T) {
	const (
		workerIP = "10.0.0.7"
		port     = 22
	)
	var (
		gotNetwork string
		gotAddr    string
		dialCalled bool
	)
	sentinel := errors.New("test sentinel: dialFn invoked")
	d := &CacheGatewayDispatcher{
		Port:        port,
		DialTimeout: 100 * time.Millisecond,
		DialFn: func(_ context.Context, network, addr string) (net.Conn, error) {
			dialCalled = true
			gotNetwork = network
			gotAddr = addr
			return nil, sentinel
		},
	}
	_, err := d.dial(t.Context(), workerIP)
	if !dialCalled {
		t.Fatal("DialFn was not invoked; dispatcher fell back to host kernel net.Dialer")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the sentinel returned by DialFn", err)
	}
	if gotNetwork != "tcp" {
		t.Errorf("DialFn network = %q, want %q", gotNetwork, "tcp")
	}
	if want := net.JoinHostPort(workerIP, "22"); gotAddr != want {
		t.Errorf("DialFn addr = %q, want %q", gotAddr, want)
	}
}

// TestCacheGatewayDispatcher_FallsBackToNetDialer covers the test-only
// fallback path: a dispatcher without DialFn uses the host net.Dialer.
// The DialTimeout makes the dial deterministic — we point at a
// guaranteed-unreachable bogon, count on the timeout firing, and
// assert the error mentions the address we passed in. Sufficient to
// prove that "no DialFn → fall back to net.Dialer" without coupling
// the test to OS-specific routing.
func TestCacheGatewayDispatcher_FallsBackToNetDialer(t *testing.T) {
	d := &CacheGatewayDispatcher{
		Port:        22,
		DialTimeout: 50 * time.Millisecond,
	}
	// 192.0.2.0/24 is TEST-NET-1 — guaranteed unreachable.
	const target = "192.0.2.1"
	_, err := d.dial(t.Context(), target)
	if err == nil {
		t.Fatal("expected dial error against unreachable host")
	}
	// Net dialer with a tight timeout surfaces a context-deadline or
	// network error containing the target host:port string.
	if want := net.JoinHostPort(target, "22"); !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want substring %q (so we know the fallback dialer ran)", err, want)
	}
}

func TestAddrFor(t *testing.T) {
	cases := []struct {
		mode string
		node Node
		want string
	}{
		{"", Node{IP: testPubIP, VPCIP: testVPCIP}, testPubIP},    // legacy default
		{"ssh", Node{IP: testPubIP, VPCIP: testVPCIP}, testPubIP}, // explicit ssh
		{TransportModeCacheGateway, Node{IP: testPubIP, VPCIP: testVPCIP}, testVPCIP},
		{TransportModeCacheGateway, Node{IP: testPubIP, VPCIP: ""}, ""}, // cache-gateway without VPC IP yields empty — caller's bug
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			o := &Orchestrator{cfg: Config{TransportMode: tc.mode}}
			got := o.addrFor(&tc.node)
			if got != tc.want {
				t.Errorf("addrFor(mode=%q, IP=%q, VPCIP=%q) = %q, want %q",
					tc.mode, tc.node.IP, tc.node.VPCIP, got, tc.want)
			}
		})
	}
}

func TestAddrForInstance(t *testing.T) {
	cases := []struct {
		mode  string
		ip4   string
		vpcIP string
		want  string
	}{
		{"", testPubIP, testVPCIP, testPubIP},
		{"ssh", testPubIP, testVPCIP, testPubIP},
		{TransportModeCacheGateway, testPubIP, testVPCIP, testVPCIP},
		{TransportModeCacheGateway, testPubIP, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			o := &Orchestrator{cfg: Config{TransportMode: tc.mode}}
			got := o.addrForInstance(tc.ip4, tc.vpcIP)
			if got != tc.want {
				t.Errorf("addrForInstance(mode=%q, ip4=%q, vpcIP=%q) = %q, want %q",
					tc.mode, tc.ip4, tc.vpcIP, got, tc.want)
			}
		})
	}
}
