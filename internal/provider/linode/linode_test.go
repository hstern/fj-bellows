package linode

import (
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
	if err := l.Configure(node); err != nil {
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
	if err := l.Configure(node); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if l.cfg.FirewallID != 12345 {
		t.Errorf("FirewallID = %d, want 12345", l.cfg.FirewallID)
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
	err := l.Configure(node)
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
	if err := l.Configure(node); err == nil {
		t.Fatal("expected error: empty allow_inbound would leave a firewall nobody can reach")
	}
}

func TestConfigureFirewallLiteralOnlyOK(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `
region: example-region
type: example-type
image: example/image
token: abc123
firewall:
  allow_inbound: ['203.0.113.5/32', '198.51.100.0/24']
`)
	if err := l.Configure(node); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if l.cfg.Firewall == nil {
		t.Fatal("Firewall block should be set")
	}
	if len(l.cfg.Firewall.AllowInbound) != 2 {
		t.Errorf("AllowInbound = %v", l.cfg.Firewall.AllowInbound)
	}
}

func TestConfigureMissingFields(t *testing.T) {
	l := &Linode{}
	node := nodeFromYAML(t, `region: example-region`)
	if err := l.Configure(node); err == nil {
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
	if err := l.Configure(node); err == nil {
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
		Tags:    []string{"fj-bellows"},
	}
	got := toInstance(in)
	if got.ID != "42" || got.Name != "fj-bellows-abcd" || got.IPv4 != "203.0.113.7" {
		t.Errorf("toInstance = %+v", got)
	}
	if !got.CreatedAt.Equal(created) || got.Tag != "fj-bellows" {
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
