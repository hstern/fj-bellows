package orchestrator

import (
	"testing"
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
