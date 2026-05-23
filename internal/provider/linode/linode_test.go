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
