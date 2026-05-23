package linode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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

// testFirewall returns a managedFirewall configured purely for
// buildRuleSet / buildExtraRule testing — the IP probe is wired to a
// fake httpDoer that errors so any unexpected `auto` resolution fails
// loudly, and the firewallClient is nil because these tests don't
// touch the Linode API.
func testFirewall(cfg firewallConfig) *managedFirewall {
	return &managedFirewall{
		cfg: cfg,
		ipProbe: externalIPProbe{
			v4URL:  "https://test/v4",
			v6URL:  "https://test/v6",
			client: stubDoer{},
		},
		log: slog.Default(),
	}
}

// mustBuildRuleSet is the test sugar for buildRuleSet — Fatals on error.
// Most tests want the rule-chunk error to propagate as a fatal, since their
// inputs are tiny and never exceed the cap.
func mustBuildRuleSet(t *testing.T, cidrs []string) linodego.FirewallRuleSet {
	t.Helper()
	rs, err := testFirewall(firewallConfig{}).buildRuleSet(context.Background(), cidrs)
	if err != nil {
		t.Fatalf("buildRuleSet(%v): %v", cidrs, err)
	}
	return rs
}

func TestBuildRuleSet(t *testing.T) {
	rs := mustBuildRuleSet(t, []string{testCIDR1, "2001:db8::1/128", "198.51.100.0/24"})
	if rs.InboundPolicy != fwInboundDrop || rs.OutboundPolicy != fwActionAccept {
		t.Errorf("policies = %q/%q, want DROP/ACCEPT", rs.InboundPolicy, rs.OutboundPolicy)
	}
	// v4 and v6 get separate rules to keep each rule under Linode's
	// 255-total-addresses-per-rule cap.
	if len(rs.Inbound) != 2 {
		t.Fatalf("want 2 inbound rules (1 v4 + 1 v6), got %d", len(rs.Inbound))
	}
	v4Rule, v6Rule := splitFamilyRules(t, rs)
	checkFamilyRule(t, v4Rule, 2, "v4")
	checkFamilyRule(t, v6Rule, 1, "v6")
}

// splitFamilyRules picks the v4-only and v6-only rules out of a built
// ruleset, asserting each is strictly one family.
func splitFamilyRules(t *testing.T, rs linodego.FirewallRuleSet) (v4, v6 *linodego.FirewallRule) {
	t.Helper()
	for i := range rs.Inbound {
		r := &rs.Inbound[i]
		v4Len := len(*r.Addresses.IPv4)
		v6Len := len(*r.Addresses.IPv6)
		switch {
		case v4Len > 0 && v6Len == 0:
			v4 = r
		case v6Len > 0 && v4Len == 0:
			v6 = r
		default:
			t.Errorf("rule %d has mixed families (v4=%d v6=%d); want strict split", i, v4Len, v6Len)
		}
	}
	if v4 == nil || v6 == nil {
		t.Fatal("want one v4-only rule and one v6-only rule")
	}
	return v4, v6
}

func checkFamilyRule(t *testing.T, r *linodego.FirewallRule, wantEntries int, family string) {
	t.Helper()
	if r.Action != fwActionAccept || r.Ports != "22" || r.Protocol != linodego.TCP {
		t.Errorf("%s rule fields off: %+v", family, r)
	}
	var got int
	if family == "v4" {
		got = len(*r.Addresses.IPv4)
	} else {
		got = len(*r.Addresses.IPv6)
	}
	if got != wantEntries {
		t.Errorf("%s rule entries = %d, want %d", family, got, wantEntries)
	}
}

func TestBuildRuleSetChunksAcrossMultipleRules(t *testing.T) {
	// 300 v4 CIDRs exceeds Linode's 255-per-rule cap; expect 2 inbound rules.
	// This is the regression for v0.2.0 CI failure: github-actions today
	// publishes >255 v4 CIDRs and the single-rule path got rejected with
	// "Too many addresses submitted. Max allowed is 255".
	cidrs := make([]string, 0, 300)
	for i := range 300 {
		cidrs = append(cidrs, fmt.Sprintf("198.51.100.%d/32", i%256))
	}
	rs, err := testFirewall(firewallConfig{}).buildRuleSet(context.Background(), cidrs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(rs.Inbound); got != 2 {
		t.Fatalf("Inbound rule count = %d, want 2 (255 + 45)", got)
	}
	if got := len(*rs.Inbound[0].Addresses.IPv4); got != 255 {
		t.Errorf("rule 0 v4 count = %d, want 255", got)
	}
	if got := len(*rs.Inbound[1].Addresses.IPv4); got != 45 {
		t.Errorf("rule 1 v4 count = %d, want 45", got)
	}
}

func TestBuildRuleSetRejectsWhenOverMaxRules(t *testing.T) {
	// 25 rules max * 255 v4 per rule = 6375 v4 capacity. 6376 trips the cap.
	cidrs := make([]string, 0, 6376)
	for i := range 6376 {
		cidrs = append(cidrs, fmt.Sprintf("203.0.113.%d/32", i%256))
	}
	if _, err := testFirewall(firewallConfig{}).buildRuleSet(context.Background(), cidrs); err == nil {
		t.Fatal("want error when allow_inbound would need more than maxRulesPerFW rules")
	}
}

// --- #46: policy overrides + extra rules ---

func TestFirewallConfigValidatePolicies(t *testing.T) {
	cases := []struct {
		name    string
		cfg     firewallConfig
		wantErr bool
	}{
		{name: "empty defaults", cfg: firewallConfig{}, wantErr: false},
		{name: "explicit valid", cfg: firewallConfig{InboundPolicy: fwActionAccept, OutboundPolicy: fwInboundDrop}, wantErr: false},
		{name: "invalid inbound", cfg: firewallConfig{InboundPolicy: "REJECT"}, wantErr: true},
		{name: "invalid outbound", cfg: firewallConfig{OutboundPolicy: "drop"}, wantErr: true}, // case-sensitive
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.validate()
			if (err != nil) != c.wantErr {
				t.Errorf("validate() err=%v, wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestExtraRuleValidate(t *testing.T) {
	cases := []struct {
		name    string
		rule    extraRule
		wantErr string
	}{
		{
			name:    "valid",
			rule:    extraRule{Label: "ok", Action: fwActionAccept, Protocol: fwProtoTCP, Ports: "22", Addresses: []string{"203.0.113.0/24"}},
			wantErr: "",
		},
		{
			name:    "missing label",
			rule:    extraRule{Action: fwActionAccept, Protocol: fwProtoTCP},
			wantErr: "label is required",
		},
		{
			name:    "bad action",
			rule:    extraRule{Label: "ok", Action: "ALLOW", Protocol: fwProtoTCP},
			wantErr: "ACCEPT or DROP",
		},
		{
			name:    "bad protocol",
			rule:    extraRule{Label: "ok", Action: fwActionAccept, Protocol: "SCTP"},
			wantErr: "TCP/UDP/ICMP/IPENCAP",
		},
		{
			name:    "non-CIDR v4",
			rule:    extraRule{Label: "ok", Action: fwActionAccept, Protocol: fwProtoTCP, Addresses: []string{"not-a-cidr"}},
			wantErr: "is not a CIDR",
		},
		// (Per-family cap now enforced post-resolution by buildExtraRule,
		// not by validate() — validate is syntactic only after the
		// sentinels-supported refactor. See TestBuildExtraRuleRejectsOverCap
		// below.)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.rule.validate()
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("want no error, got %v", err)
			case c.wantErr != "" && err == nil:
				t.Errorf("want error containing %q, got nil", c.wantErr)
			case c.wantErr != "" && err != nil && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("err = %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

func TestBuildRuleSetHonoursPolicyOverrides(t *testing.T) {
	cfg := firewallConfig{
		InboundPolicy:  fwActionAccept, // override the default DROP
		OutboundPolicy: fwInboundDrop,  // override the default ACCEPT
	}
	rs, err := testFirewall(cfg).buildRuleSet(context.Background(), []string{testCIDR1})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	if rs.InboundPolicy != fwActionAccept {
		t.Errorf("InboundPolicy = %q, want ACCEPT", rs.InboundPolicy)
	}
	if rs.OutboundPolicy != fwInboundDrop {
		t.Errorf("OutboundPolicy = %q, want DROP", rs.OutboundPolicy)
	}
}

func TestBuildRuleSetAppendsExtraInboundAfterSynth(t *testing.T) {
	extra := extraRule{
		Label:     "prometheus",
		Action:    fwActionAccept,
		Protocol:  fwProtoTCP,
		Ports:     "9100",
		Addresses: []string{"10.0.0.0/8"},
	}
	cfg := firewallConfig{ExtraInbound: []extraRule{extra}}
	rs, err := testFirewall(cfg).buildRuleSet(context.Background(), []string{testCIDR1})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	// 1 synth (v4 only — testCIDR1 is v4) + 1 extra = 2 inbound.
	if len(rs.Inbound) != 2 {
		t.Fatalf("want 2 inbound rules (1 synth + 1 extra), got %d", len(rs.Inbound))
	}
	last := rs.Inbound[len(rs.Inbound)-1]
	if last.Label != "prometheus" {
		t.Errorf("extra rule should be last; got label %q", last.Label)
	}
	if last.Ports != "9100" {
		t.Errorf("extra rule ports = %q, want 9100", last.Ports)
	}
}

func TestBuildRuleSetExtraOutboundReplacesEmptyDefault(t *testing.T) {
	extra := extraRule{
		Label:     "deny-smtp",
		Action:    fwInboundDrop,
		Protocol:  fwProtoTCP,
		Ports:     "25",
		Addresses: []string{anyV4CIDR, anyV6CIDR},
	}
	cfg := firewallConfig{ExtraOutbound: []extraRule{extra}}
	rs, err := testFirewall(cfg).buildRuleSet(context.Background(), []string{testCIDR1})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	if len(rs.Outbound) != 1 {
		t.Fatalf("want 1 outbound rule, got %d", len(rs.Outbound))
	}
	if rs.Outbound[0].Label != "deny-smtp" || rs.Outbound[0].Action != fwInboundDrop {
		t.Errorf("outbound rule wrong: %+v", rs.Outbound[0])
	}
}

func TestBuildRuleSetRejectsWhenSynthPlusExtrasOverCap(t *testing.T) {
	// One synth v4 rule + 25 extras = 26 inbound rules, over the 25 cap.
	extras := make([]extraRule, 25)
	for i := range extras {
		extras[i] = extraRule{
			Label:     fmt.Sprintf("extra-%d", i),
			Action:    fwActionAccept,
			Protocol:  fwProtoTCP,
			Ports:     strconv.Itoa(10000 + i),
			Addresses: []string{"203.0.113.0/24"},
		}
	}
	cfg := firewallConfig{ExtraInbound: extras}
	if _, err := testFirewall(cfg).buildRuleSet(context.Background(), []string{testCIDR1}); err == nil {
		t.Fatal("want error when synth + extras exceed Linode's 25-rule cap")
	}
}

func TestBuildRuleSetIPv6ExtrasRoundTrip(t *testing.T) {
	// Operator supplies an IPv6-only rule. Make sure both family slices land
	// in the linodego rule (one populated, one empty-but-non-nil) so the
	// Linode API JSON-encodes them.
	cfg := firewallConfig{
		ExtraInbound: []extraRule{{
			Label:     "v6-only",
			Action:    fwActionAccept,
			Protocol:  fwProtoTCP,
			Ports:     "443",
			Addresses: []string{"2001:db8::/32"},
		}},
	}
	rs, err := testFirewall(cfg).buildRuleSet(context.Background(), nil)
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	// allow_inbound was nil -> 1 placeholder synth rule + 1 extra = 2.
	if len(rs.Inbound) != 2 {
		t.Fatalf("want 2 inbound rules (placeholder + extra), got %d", len(rs.Inbound))
	}
	extra := rs.Inbound[1]
	if extra.Addresses.IPv4 == nil || extra.Addresses.IPv6 == nil {
		t.Fatalf("both family slices must be non-nil; got %+v", extra.Addresses)
	}
	if got := *extra.Addresses.IPv6; len(got) != 1 || got[0] != "2001:db8::/32" {
		t.Errorf("v6 addresses = %v, want [2001:db8::/32]", got)
	}
	if got := *extra.Addresses.IPv4; len(got) != 0 {
		t.Errorf("v4 addresses should be empty, got %v", got)
	}
}

func TestRuleSetAddrsEqual(t *testing.T) {
	a := mustBuildRuleSet(t, []string{testCIDR1})
	b := mustBuildRuleSet(t, []string{testCIDR1})
	if !ruleSetAddrsEqual(a, b) {
		t.Error("identical rulesets compared unequal")
	}
	c := mustBuildRuleSet(t, []string{testCIDR3})
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
	id, err := m.ensureFirewall(context.Background(), mustBuildRuleSet(t, []string{testCIDR1}))
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
	rs := mustBuildRuleSet(t, []string{testCIDR1})
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
	_, _ = m.ensureFirewall(context.Background(), mustBuildRuleSet(t, []string{testCIDR1}))
	_, _ = m.ensureFirewall(context.Background(), mustBuildRuleSet(t, []string{testCIDR1, testCIDR4}))
	if fake.updateCalls != 1 {
		t.Errorf("updateCalls = %d, want 1 (rules drifted)", fake.updateCalls)
	}
}

func TestEnsureFirewallTagIsolation(t *testing.T) {
	// Two deployments on the same Linode account get distinct firewalls.
	fake := newFakeFirewallClient()
	a := &managedFirewall{tag: "deploy-a", client: fake, log: slog.Default()}
	b := &managedFirewall{tag: "deploy-b", client: fake, log: slog.Default()}
	idA, _ := a.ensureFirewall(context.Background(), mustBuildRuleSet(t, []string{testCIDR1}))
	idB, _ := b.ensureFirewall(context.Background(), mustBuildRuleSet(t, []string{testCIDR1}))
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
	id, _ := m.ensureFirewall(context.Background(), mustBuildRuleSet(t, []string{testCIDR1}))
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
	id, _ := m.ensureFirewall(context.Background(), mustBuildRuleSet(t, []string{testCIDR1}))
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

func TestResolveAllowInboundLiteralPlusAuto(t *testing.T) {
	httpStub := stubDoer{
		testV4URL: {body: "203.0.113.99\n"},
		testV6URL: {err: errors.New("no v6")},
	}
	m := &managedFirewall{
		cfg: firewallConfig{
			AllowInbound: []string{testCIDR2, sentinelAuto},
		},
		ipProbe: externalIPProbe{v4URL: testV4URL, v6URL: testV6URL, client: httpStub},
		log:     slog.Default(),
	}
	cidrs, err := m.resolveAllowInbound(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cidrs) != 2 {
		t.Errorf("want 2 cidrs (literal + v4 auto), got %d: %v", len(cidrs), cidrs)
	}
}

func TestResolveAllowInboundUnknownSentinelFails(t *testing.T) {
	m := &managedFirewall{
		cfg: firewallConfig{
			AllowInbound: []string{"github-actions"}, // removed in v0.2.1 — must fail now
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
			AllowInbound: []string{testCIDR2, sentinelAuto},
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
			AllowInbound: []string{testCIDR2, sentinelAuto},
		},
		tag:     testTag,
		client:  fake,
		ipProbe: externalIPProbe{v4URL: testV4URL, v6URL: testV6URL, client: failingHTTP},
		log:     slog.Default(),
	}
	// Seed with a previously-applied ruleset.
	original := []string{testCIDR2}
	id, err := m.ensureFirewall(context.Background(), mustBuildRuleSet(t, original))
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

// --- sentinel coverage across BOTH surfaces (allow_inbound + extras) ---

// testFirewallWithProbe lets sentinel tests inject a stubDoer for the IP
// probe so `auto` resolves deterministically without hitting icanhazip.
func testFirewallWithProbe(cfg firewallConfig, doer stubDoer) *managedFirewall {
	return &managedFirewall{
		cfg: cfg,
		ipProbe: externalIPProbe{
			v4URL:  testV4URL,
			v6URL:  testV6URL,
			client: doer,
		},
		log: slog.Default(),
	}
}

func TestResolveSentinelsAnyVariants(t *testing.T) {
	m := testFirewall(firewallConfig{}) // probe never consulted for the any* set
	got, err := m.resolveSentinels(context.Background(), []string{"any-v4", "any-v6", "any", testCIDR4})
	if err != nil {
		t.Fatalf("resolveSentinels: %v", err)
	}
	want := []string{anyV4CIDR, anyV6CIDR, anyV4CIDR, anyV6CIDR, testCIDR4}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveSentinelsAutoUsesProbe(t *testing.T) {
	doer := stubDoer{
		testV4URL: {body: "203.0.113.99\n"},
		testV6URL: {err: errors.New("no v6")},
	}
	m := testFirewallWithProbe(firewallConfig{}, doer)
	got, err := m.resolveSentinels(context.Background(), []string{sentinelAuto})
	if err != nil {
		t.Fatalf("resolveSentinels: %v", err)
	}
	if len(got) != 1 || got[0] != "203.0.113.99/32" {
		t.Errorf("got %v, want [203.0.113.99/32]", got)
	}
}

func TestResolveSentinelsRejectsTypo(t *testing.T) {
	m := testFirewall(firewallConfig{})
	_, err := m.resolveSentinels(context.Background(), []string{"any-v5"})
	if err == nil {
		t.Fatal("want error on unknown sentinel")
	}
	if !strings.Contains(err.Error(), "auto, any, any-v4, any-v6") {
		t.Errorf("error should list the supported sentinels, got: %v", err)
	}
}

func TestAllowInboundAcceptsAnySentinels(t *testing.T) {
	m := testFirewall(firewallConfig{
		AllowInbound: []string{"any-v4", testCIDR4},
	})
	got, err := m.resolveAllowInbound(context.Background())
	if err != nil {
		t.Fatalf("resolveAllowInbound: %v", err)
	}
	// Sorted+deduped.
	want := []string{anyV4CIDR, testCIDR4}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtraRuleAcceptsAutoSentinel(t *testing.T) {
	doer := stubDoer{
		testV4URL: {body: testIP4Body},
		testV6URL: {err: errors.New("no v6")},
	}
	m := testFirewallWithProbe(firewallConfig{
		ExtraInbound: []extraRule{{
			Label:     "ssh-from-me",
			Action:    fwActionAccept,
			Protocol:  fwProtoTCP,
			Ports:     "22",
			Addresses: []string{sentinelAuto},
		}},
	}, doer)
	rs, err := m.buildRuleSet(context.Background(), []string{testCIDR1})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	// 1 synth (v4 only) + 1 extra = 2 inbound.
	if len(rs.Inbound) != 2 {
		t.Fatalf("want 2 inbound rules, got %d", len(rs.Inbound))
	}
	extra := rs.Inbound[1]
	if got := *extra.Addresses.IPv4; len(got) != 1 || got[0] != testCIDR3 {
		t.Errorf("extra v4 addresses = %v, want [203.0.113.10/32]", got)
	}
}

func TestExtraRuleAcceptsAnySentinel(t *testing.T) {
	m := testFirewall(firewallConfig{
		ExtraOutbound: []extraRule{{
			Label:     "deny-smtp",
			Action:    fwInboundDrop,
			Protocol:  fwProtoTCP,
			Ports:     "25",
			Addresses: []string{"any"},
		}},
	})
	rs, err := m.buildRuleSet(context.Background(), []string{testCIDR1})
	if err != nil {
		t.Fatalf("buildRuleSet: %v", err)
	}
	if len(rs.Outbound) != 1 {
		t.Fatalf("want 1 outbound rule, got %d", len(rs.Outbound))
	}
	out := rs.Outbound[0]
	if got := *out.Addresses.IPv4; len(got) != 1 || got[0] != anyV4CIDR {
		t.Errorf("v4 = %v, want [0.0.0.0/0]", got)
	}
	if got := *out.Addresses.IPv6; len(got) != 1 || got[0] != anyV6CIDR {
		t.Errorf("v6 = %v, want [::/0]", got)
	}
}

func TestExtraRuleValidateAcceptsSentinelTokens(t *testing.T) {
	// validate is syntactic only — sentinels pass without touching the
	// probe. Per-family cap enforcement happens later in buildExtraRule.
	for _, sent := range []string{"auto", "any", "any-v4", "any-v6"} {
		r := extraRule{Label: "ok", Action: fwActionAccept, Protocol: fwProtoTCP, Addresses: []string{sent}}
		if err := r.validate(); err != nil {
			t.Errorf("validate rejected sentinel %q: %v", sent, err)
		}
	}
}

func TestBuildExtraRuleRejectsOverCap(t *testing.T) {
	addrs := make([]string, 256)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("198.51.100.%d/32", i)
	}
	m := testFirewall(firewallConfig{})
	_, err := m.buildExtraRule(context.Background(), extraRule{
		Label:     "too-many",
		Action:    fwActionAccept,
		Protocol:  fwProtoTCP,
		Addresses: addrs,
	})
	if err == nil {
		t.Fatal("want error when extra rule resolves to >255 v4 addresses")
	}
	if !strings.Contains(err.Error(), "caps a rule at") {
		t.Errorf("err = %v, want substring 'caps a rule at'", err)
	}
}
