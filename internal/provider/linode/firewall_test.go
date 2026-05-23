package linode

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/linode/linodego"
)

// fakeFirewallClient is a hand-rolled firewallClient. Stores firewalls in a
// map keyed by ID, devices keyed by firewall ID. Records call counts so tests
// can assert the right operations fired.
type fakeFirewallClient struct {
	mu        sync.Mutex
	firewalls map[int]*linodego.Firewall
	devices   map[int][]linodego.FirewallDevice
	nextID    int

	listCalls    int
	createCalls  int
	updateCalls  int
	deleteCalls  int
	listDevCalls int
}

func newFakeFirewallClient() *fakeFirewallClient {
	return &fakeFirewallClient{
		firewalls: map[int]*linodego.Firewall{},
		devices:   map[int][]linodego.FirewallDevice{},
	}
}

func (f *fakeFirewallClient) ListFirewalls(_ context.Context, _ *linodego.ListOptions) ([]linodego.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	out := make([]linodego.Firewall, 0, len(f.firewalls))
	for _, v := range f.firewalls {
		out = append(out, *v)
	}
	return out, nil
}

func (f *fakeFirewallClient) CreateFirewall(_ context.Context, opts linodego.FirewallCreateOptions) (*linodego.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.nextID++
	fw := &linodego.Firewall{
		ID:    f.nextID,
		Label: opts.Label,
		Tags:  opts.Tags,
		Rules: opts.Rules,
	}
	f.firewalls[fw.ID] = fw
	return fw, nil
}

func (f *fakeFirewallClient) UpdateFirewallRules(_ context.Context, id int, rules linodego.FirewallRuleSet) (*linodego.FirewallRuleSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	fw, ok := f.firewalls[id]
	if !ok {
		return nil, errors.New("not found")
	}
	fw.Rules = rules
	return &rules, nil
}

func (f *fakeFirewallClient) DeleteFirewall(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	delete(f.firewalls, id)
	delete(f.devices, id)
	return nil
}

func (f *fakeFirewallClient) ListFirewallDevices(_ context.Context, id int, _ *linodego.ListOptions) ([]linodego.FirewallDevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listDevCalls++
	return append([]linodego.FirewallDevice(nil), f.devices[id]...), nil
}

func TestFirewallLabel(t *testing.T) {
	cases := []struct {
		in       string
		minLen   int
		maxLen   int
		mustHave string // substring that must appear (e.g. sanitized prefix)
	}{
		{in: "fj-bellows", minLen: 3, maxLen: 32, mustHave: "fj-bellows-fj-bellows"},
		{in: "deploy.one_two-3", minLen: 3, maxLen: 32, mustHave: "fj-bellows-deploy.one_two-3"},
		// Invalid chars get replaced with '-'.
		{in: "weird/chars!", minLen: 3, maxLen: 32, mustHave: "fj-bellows-weird-chars-"},
		// Long tag → sanitized, truncated, with hash suffix for uniqueness.
		{
			in:       strings.Repeat("a", 64),
			minLen:   3,
			maxLen:   32,
			mustHave: "fj-bellows-",
		},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := firewallLabel(c.in)
			if len(got) < c.minLen || len(got) > c.maxLen {
				t.Errorf("len(%q) = %d, want between %d and %d", got, len(got), c.minLen, c.maxLen)
			}
			if !strings.Contains(got, c.mustHave) {
				t.Errorf("got %q, want substring %q", got, c.mustHave)
			}
		})
	}
}

func TestFirewallLabelDifferentLongTagsDontCollide(t *testing.T) {
	a := firewallLabel(strings.Repeat("a", 64))
	b := firewallLabel(strings.Repeat("b", 64))
	if a == b {
		t.Errorf("two distinct 64-char tags collided: both → %q", a)
	}
}

func TestBuildRuleSet(t *testing.T) {
	rs := buildRuleSet([]string{testCIDR1, "2001:db8::1/128", "198.51.100.0/24"})
	if rs.InboundPolicy != "DROP" {
		t.Errorf("InboundPolicy = %q, want DROP", rs.InboundPolicy)
	}
	if rs.OutboundPolicy != "ACCEPT" {
		t.Errorf("OutboundPolicy = %q, want ACCEPT", rs.OutboundPolicy)
	}
	if len(rs.Inbound) != 1 {
		t.Fatalf("want exactly one inbound rule, got %d", len(rs.Inbound))
	}
	rule := rs.Inbound[0]
	if rule.Action != "ACCEPT" {
		t.Errorf("Action = %q, want ACCEPT", rule.Action)
	}
	if rule.Ports != "22" {
		t.Errorf("Ports = %q, want 22", rule.Ports)
	}
	if rule.Protocol != linodego.TCP {
		t.Errorf("Protocol = %q, want TCP", rule.Protocol)
	}
	v4 := *rule.Addresses.IPv4
	v6 := *rule.Addresses.IPv6
	if len(v4) != 2 || len(v6) != 1 {
		t.Errorf("bucket split wrong: v4=%v v6=%v (want 2 v4, 1 v6)", v4, v6)
	}
}

func TestRuleSetAddrsEqual(t *testing.T) {
	a := buildRuleSet([]string{testCIDR1})
	b := buildRuleSet([]string{testCIDR1})
	if !ruleSetAddrsEqual(a, b) {
		t.Error("identical rulesets compared unequal")
	}
	c := buildRuleSet([]string{"203.0.113.10/32"})
	if ruleSetAddrsEqual(a, c) {
		t.Error("different rulesets compared equal")
	}
}

func TestEnsureFirewallCreatesWhenAbsent(t *testing.T) {
	fake := newFakeFirewallClient()
	m := &managedFirewall{
		tag:    testTag,
		client: fake,
		log:    slog.Default(),
	}
	id, err := m.ensureFirewall(context.Background(), buildRuleSet([]string{testCIDR1}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero ID")
	}
	if fake.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", fake.createCalls)
	}
	if got := fake.firewalls[id].Label; got != firewallLabel(testTag) {
		t.Errorf("label = %q, want %q", got, firewallLabel(testTag))
	}
}

func TestEnsureFirewallReusesWhenSameTagPresent(t *testing.T) {
	fake := newFakeFirewallClient()
	m := &managedFirewall{
		tag:    testTag,
		client: fake,
		log:    slog.Default(),
	}
	rs := buildRuleSet([]string{testCIDR1})
	id1, _ := m.ensureFirewall(context.Background(), rs)
	id2, _ := m.ensureFirewall(context.Background(), rs)
	if id1 != id2 {
		t.Errorf("ids differ: %d vs %d (should reuse the existing firewall)", id1, id2)
	}
	if fake.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (no second create)", fake.createCalls)
	}
	if fake.updateCalls != 0 {
		t.Errorf("updateCalls = %d, want 0 (rules unchanged)", fake.updateCalls)
	}
}

func TestEnsureFirewallUpdatesOnDrift(t *testing.T) {
	fake := newFakeFirewallClient()
	m := &managedFirewall{
		tag:    testTag,
		client: fake,
		log:    slog.Default(),
	}
	_, _ = m.ensureFirewall(context.Background(), buildRuleSet([]string{testCIDR1}))
	_, _ = m.ensureFirewall(context.Background(), buildRuleSet([]string{testCIDR1, "203.0.113.5/32"}))
	if fake.updateCalls != 1 {
		t.Errorf("updateCalls = %d, want 1 (rules drifted)", fake.updateCalls)
	}
}

func TestEnsureFirewallTagIsolation(t *testing.T) {
	// Two deployments on the same Linode account get distinct firewalls.
	fake := newFakeFirewallClient()
	a := &managedFirewall{tag: "deploy-a", client: fake, log: slog.Default()}
	b := &managedFirewall{tag: "deploy-b", client: fake, log: slog.Default()}
	idA, _ := a.ensureFirewall(context.Background(), buildRuleSet([]string{testCIDR1}))
	idB, _ := b.ensureFirewall(context.Background(), buildRuleSet([]string{testCIDR1}))
	if idA == idB {
		t.Errorf("two distinct tags collided on the same firewall (%d)", idA)
	}
	if fake.createCalls != 2 {
		t.Errorf("createCalls = %d, want 2", fake.createCalls)
	}
}

func TestMaybeCleanupFirewallDeletesWhenEmpty(t *testing.T) {
	fake := newFakeFirewallClient()
	m := &managedFirewall{tag: testTag, client: fake, log: slog.Default()}
	id, _ := m.ensureFirewall(context.Background(), buildRuleSet([]string{testCIDR1}))
	m.id = id
	m.maybeCleanupFirewall(context.Background())
	if fake.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", fake.deleteCalls)
	}
	if _, exists := fake.firewalls[id]; exists {
		t.Error("firewall still in fake store after cleanup")
	}
}

func TestMaybeCleanupFirewallSkipsWhenDevicesAttached(t *testing.T) {
	fake := newFakeFirewallClient()
	m := &managedFirewall{tag: testTag, client: fake, log: slog.Default()}
	id, _ := m.ensureFirewall(context.Background(), buildRuleSet([]string{testCIDR1}))
	fake.devices[id] = []linodego.FirewallDevice{{ID: 99}}
	m.maybeCleanupFirewall(context.Background())
	if fake.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0 (devices still attached)", fake.deleteCalls)
	}
}

func TestMaybeCleanupFirewallNoOpWhenNotFound(t *testing.T) {
	fake := newFakeFirewallClient()
	m := &managedFirewall{tag: testTag, client: fake, log: slog.Default()}
	// No ensureFirewall call → nothing to clean up.
	m.maybeCleanupFirewall(context.Background())
	if fake.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0", fake.deleteCalls)
	}
}

func TestResolveAllowInboundLiteralPlusSentinels(t *testing.T) {
	httpStub := stubDoer{
		testV4URL:              {body: "203.0.113.99\n"},
		testV6URL:              {err: errors.New("no v6")},
		"https://meta.example": {body: `{"actions":["192.0.2.0/24"]}`},
	}
	m := &managedFirewall{
		cfg: firewallConfig{
			AllowInbound: []string{testCIDR2, "auto", "github-actions"},
		},
		ipProbe:   externalIPProbe{v4URL: testV4URL, v6URL: testV6URL, client: httpStub},
		ghMetaURL: "https://meta.example",
		httpDoer:  httpStub,
		log:       slog.Default(),
	}
	cidrs, err := m.resolveAllowInbound(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cidrs) != 3 {
		t.Errorf("want 3 cidrs (literal + v4 auto + 1 GH), got %d: %v", len(cidrs), cidrs)
	}
}

func TestResolveAllowInboundUnknownSentinelFails(t *testing.T) {
	m := &managedFirewall{
		cfg: firewallConfig{
			AllowInbound: []string{"gha"}, // typo of github-actions
		},
		log: slog.Default(),
	}
	if _, err := m.resolveAllowInbound(context.Background()); err == nil {
		t.Fatal("want error on unknown sentinel")
	}
}

func TestResolveAllowInboundSentinelFailureIsFatal(t *testing.T) {
	httpStub := stubDoer{
		testV4URL: {err: errors.New("v4 down")},
		testV6URL: {err: errors.New("v6 down")},
	}
	m := &managedFirewall{
		cfg: firewallConfig{
			AllowInbound: []string{testCIDR2, "auto"},
		},
		ipProbe: externalIPProbe{v4URL: testV4URL, v6URL: testV6URL, client: httpStub},
		log:     slog.Default(),
	}
	if _, err := m.resolveAllowInbound(context.Background()); err == nil {
		t.Fatal("want error when auto sentinel fully fails (don't silently drop)")
	}
}

func TestRefreshOnceKeepsExistingRulesOnFailure(t *testing.T) {
	// Pre-populate with a working ruleset, then make the next refresh fail.
	// Verify the firewall's rules are unchanged.
	fake := newFakeFirewallClient()
	failingHTTP := stubDoer{
		testV4URL: {err: errors.New("v4 down")},
		testV6URL: {err: errors.New("v6 down")},
	}
	m := &managedFirewall{
		cfg: firewallConfig{
			AllowInbound: []string{testCIDR2, "auto"},
		},
		tag:     testTag,
		client:  fake,
		ipProbe: externalIPProbe{v4URL: testV4URL, v6URL: testV6URL, client: failingHTTP},
		log:     slog.Default(),
	}
	// Seed with a previously-applied ruleset.
	original := []string{testCIDR2}
	id, err := m.ensureFirewall(context.Background(), buildRuleSet(original))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	m.id = id
	m.lastApplied = original
	beforeUpdate := fake.updateCalls

	m.refreshOnce(context.Background())

	if fake.updateCalls != beforeUpdate {
		t.Errorf("UpdateFirewallRules called %d times during refresh; want %d (failure must keep existing rules)",
			fake.updateCalls-beforeUpdate, 0)
	}
}

// Avoid a fmt.Stringer interface mismatch when slog formats the body — keep
// the imports honest.
var _ = io.Discard

// Compile-time sanity that linodego.Client itself satisfies firewallClient.
var _ firewallClient = (*linodego.Client)(nil)

// fakeFirewallClient satisfies the interface.
var _ firewallClient = (*fakeFirewallClient)(nil)

// http import kept for the stubDoer test file (separate package member).
var _ = http.MethodGet
