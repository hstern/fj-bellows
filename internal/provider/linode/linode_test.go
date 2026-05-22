package linode

import (
	"net"
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
