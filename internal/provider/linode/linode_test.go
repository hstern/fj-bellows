package linode

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/linode/linodego"
	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

func nodeFromYAML(t *testing.T, s string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(s), &n); err != nil {
		t.Fatal(err)
	}
	// Unmarshal wraps in a document node; descend to the mapping.
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return *n.Content[0]
	}
	return n
}

func TestConfigure(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
`)
	if err := l.Configure(context.Background(), "testtag", node); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if l.cfg.Region != "example-region" || l.cfg.Type != "example-type" {
		t.Errorf("cfg = %+v", l.cfg)
	}
	if l.cfg.FirewallID != 0 {
		t.Errorf("FirewallID = %d, want 0 (unset)", l.cfg.FirewallID)
	}
}

func TestConfigureFirewallID(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
firewall_id: 12345
`)
	if err := l.Configure(context.Background(), "testtag", node); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if l.cfg.FirewallID != 12345 {
		t.Errorf("FirewallID = %d, want 12345", l.cfg.FirewallID)
	}
}

func TestConfigurePlacementGroupAndPlacementGroupIDAreMutuallyExclusive(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
placement_group_id: 999
placement_group:
  enforcement: strict
`)
	err := l.Configure(context.Background(), "testtag", node)
	if err == nil {
		t.Fatal("expected error when both placement_group and placement_group_id are set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %v should mention mutual exclusion", err)
	}
}

func TestConfigurePlacementGroupBadEnforcement(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
placement_group:
  enforcement: loose
`)
	err := l.Configure(context.Background(), "testtag", node)
	if err == nil {
		t.Fatal("expected error on invalid enforcement value")
	}
	if !strings.Contains(err.Error(), "enforcement") {
		t.Errorf("error should mention enforcement, got: %v", err)
	}
}

func TestConfigurePlacementGroupIDOnlyDecodes(t *testing.T) {
	// The attach-by-ID path doesn't hit the placement-group API at
	// Configure time, so a fake token doesn't error here. Verifies the
	// YAML decodes onto cfg.PlacementGroupID and the mutex check
	// doesn't fire when only one mode is set.
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
placement_group_id: 12345
`)
	if err := l.Configure(context.Background(), "testtag", node); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if l.cfg.PlacementGroupID != 12345 {
		t.Errorf("PlacementGroupID = %d, want 12345", l.cfg.PlacementGroupID)
	}
	if l.cfg.PlacementGroup != nil {
		t.Errorf("PlacementGroup should be nil, got %+v", l.cfg.PlacementGroup)
	}
}

func TestConfigureFirewallAndFirewallIDAreMutuallyExclusive(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
firewall_id: 12345
firewall:
  allow_inbound: ['203.0.113.5/32']
`)
	err := l.Configure(context.Background(), "testtag", node)
	if err == nil {
		t.Fatal("expected error when both firewall and firewall_id are set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %v should mention mutual exclusion", err)
	}
}

func TestConfigureFirewallEmptyAllowInboundIsFatal(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
firewall:
  allow_inbound: []
`)
	if err := l.Configure(context.Background(), "testtag", node); err == nil {
		t.Fatal("expected error: empty allow_inbound would leave a firewall nobody can reach")
	}
}

func TestConfigureFirewallLiteralReachesAPI(t *testing.T) {
	// With eager-create, a valid literal-CIDR firewall block makes Configure
	// reach the Linode firewall API. We can't fake the linodego.Client at
	// this layer, so we use a deliberately-fake token and assert Configure
	// gets past YAML decode + the mutex check + sentinel validation, and
	// the only failure is from the (real, unauthenticated) API call. The
	// firewall-API behaviour itself is covered by firewall_test.go against
	// the fakeFirewallClient.
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
firewall:
  allow_inbound: ['203.0.113.5/32', '198.51.100.0/24']
`)
	err := l.Configure(context.Background(), "testtag", node)
	if err == nil {
		t.Fatal("expected error: fake token can't authenticate to the Linode API")
	}
	if !strings.Contains(err.Error(), "firewall") {
		t.Errorf("error should be firewall-API-related, got: %v", err)
	}
	if l.cfg.Firewall == nil {
		t.Fatal("Firewall block should still be decoded onto cfg even on API error")
	}
	if len(l.cfg.Firewall.AllowInbound) != 2 {
		t.Errorf("AllowInbound = %v", l.cfg.Firewall.AllowInbound)
	}
}

func TestConfigureVPCBadSubnetCIDR(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
vpc:
  subnets:
    cache:
      ipv4: not-a-cidr
`)
	err := l.Configure(context.Background(), "testtag", node)
	if err == nil {
		t.Fatal("expected error on invalid subnet CIDR")
	}
	if !strings.Contains(err.Error(), "vpc") || !strings.Contains(err.Error(), "CIDR") {
		t.Errorf("error should mention vpc + CIDR, got: %v", err)
	}
}

func TestConfigureVPCEmptySubnetsIsFatal(t *testing.T) {
	// Same posture as the firewall's empty-allow-inbound check: a vpc
	// block with no subnets is a misconfiguration we refuse at Configure
	// rather than silently producing a VPC that workers can't attach to.
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
vpc: {}
`)
	if err := l.Configure(context.Background(), "testtag", node); err == nil {
		t.Fatal("expected error: vpc block with no subnets is misconfiguration")
	}
}

func TestConfigureVPCWorkerSubnetNotDeclared(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
vpc:
  subnets:
    cache:
      ipv4: 100.64.0.0/24
  worker_subnet: nonexistent
`)
	err := l.Configure(context.Background(), "testtag", node)
	if err == nil {
		t.Fatal("expected error when worker_subnet references an undeclared subnet")
	}
	if !strings.Contains(err.Error(), "worker_subnet") {
		t.Errorf("error should mention worker_subnet, got: %v", err)
	}
}

func TestConfigureVPCLiteralReachesAPI(t *testing.T) {
	// Eager-create: a valid vpc block makes Configure call the Linode
	// VPC API. With a fake token the API call fails — we just want to
	// see Configure got past validate() into the API layer, proving the
	// VPC path is wired. The VPC API behaviour itself is covered by
	// vpc_test.go against the fakeVPCClient.
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
vpc:
  subnets:
    cache:
      ipv4: 100.64.0.0/24
`)
	err := l.Configure(context.Background(), "testtag", node)
	if err == nil {
		t.Fatal("expected error: fake token can't authenticate to the Linode API")
	}
	if !strings.Contains(err.Error(), "vpc") {
		t.Errorf("error should be vpc-API-related, got: %v", err)
	}
	if l.cfg.VPC == nil {
		t.Fatal("VPC block should still be decoded onto cfg even on API error")
	}
	if _, ok := l.cfg.VPC.Subnets["cache"]; !ok {
		t.Errorf("Subnets[cache] should be decoded, got %+v", l.cfg.VPC.Subnets)
	}
}

func TestConfigureCacheAutoSynthesizesVPC(t *testing.T) {
	// `cache: {}` without `vpc:` must auto-populate cfg.VPC so workers
	// can reach the cache over a private NIC. The auto-synthesis runs
	// at Configure-time after validateAll, before setupManagedVPC.
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
cache: {}
`)
	err := l.Configure(context.Background(), "testtag", node)
	if err == nil {
		t.Fatal("expected error from VPC create with fake token")
	}
	// Even though the API call fails, the auto-synthesis should have
	// run and the VPC config should be populated on l.cfg.
	if l.cfg.VPC == nil {
		t.Fatal("cache: {} did not auto-synthesize cfg.VPC")
	}
	sub, ok := l.cfg.VPC.Subnets[defaultCacheSubnetName]
	if !ok {
		t.Fatalf("auto-synthesized VPC missing %q subnet: %+v", defaultCacheSubnetName, l.cfg.VPC.Subnets)
	}
	if sub.IPv4 != defaultCacheSubnetCIDR {
		t.Errorf("synthesized subnet IPv4 = %q, want %q", sub.IPv4, defaultCacheSubnetCIDR)
	}
}

func TestConfigureCachePreservesExplicitVPC(t *testing.T) {
	// When an operator provides both cache: and vpc:, the explicit
	// vpc: must win — auto-synthesis must not silently overwrite the
	// operator's CIDR choice.
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
vpc:
  subnets:
    cache:
      ipv4: 192.168.55.0/24
cache: {}
`)
	_ = l.Configure(context.Background(), "testtag", node) // API failure is expected
	if l.cfg.VPC == nil {
		t.Fatal("explicit vpc: lost during decode")
	}
	sub := l.cfg.VPC.Subnets["cache"]
	if sub.IPv4 != "192.168.55.0/24" {
		t.Errorf("explicit subnet IPv4 = %q, want %q (auto-synthesis must not clobber operator choice)",
			sub.IPv4, "192.168.55.0/24")
	}
}

func TestConfigureCacheReachesAPI(t *testing.T) {
	// Valid cache config → Configure tries to call the Linode VPC API
	// (synthesized) and fails with the fake token. We just want to
	// see Configure got past validate() + auto-synthesis into the
	// API layer, proving the cache path is wired.
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
cache: {}
`)
	err := l.Configure(context.Background(), "testtag", node)
	if err == nil {
		t.Fatal("expected error: fake token can't authenticate")
	}
	if l.cfg.Cache == nil {
		t.Fatal("cache block should decode onto cfg even on API error")
	}
}

func TestConfigureMissingFields(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `region: example-region`)
	if err := l.Configure(context.Background(), "testtag", node); err == nil {
		t.Fatal("expected error for missing type/image/token")
	}
}

func TestConfigureMissingToken(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `
region: r
type: t
image: i
`)
	if err := l.Configure(context.Background(), "testtag", node); err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestBillingModel(t *testing.T) {
	l := &Linode{}
	if l.BillingModel() != provider.BillingHourlyRoundUp {
		t.Errorf("BillingModel = %v", l.BillingModel())
	}
}

func TestRegisteredInRegistry(t *testing.T) {
	p, err := provider.New("linode")
	if err != nil {
		t.Fatalf("linode not registered: %v", err)
	}
	if _, ok := p.(*Linode); !ok {
		t.Errorf("registry returned %T", p)
	}
}

func TestToInstance(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ip := net.ParseIP("203.0.113.7")
	in := linodego.Instance{
		ID:      42,
		Label:   "fj-bellows-abcd",
		IPv4:    []*net.IP{&ip},
		Created: &created,
		Tags:    []string{testLabelPrefix},
	}
	got := toInstance(in)
	if got.ID != "42" || got.Name != "fj-bellows-abcd" || got.IPv4 != "203.0.113.7" {
		t.Errorf("toInstance = %+v", got)
	}
	if !got.CreatedAt.Equal(created) || got.Tag != testLabelPrefix {
		t.Errorf("toInstance time/tag = %+v", got)
	}
}

func TestRandomPassword(t *testing.T) {
	p1, err := randomPassword(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 32 {
		t.Errorf("len = %d", len(p1))
	}
	p2, _ := randomPassword(32)
	if p1 == p2 {
		t.Error("passwords should differ")
	}
}
