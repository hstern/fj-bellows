package linode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/linode/linodego"
)

// Linode firewall action + protocol strings, centralised so lint doesn't
// flag the repeated literals and the supported set is easy to extend.
const (
	fwActionAccept   = "ACCEPT"
	fwInboundDrop    = "DROP"
	fwOutboundAccept = "ACCEPT"

	fwProtoTCP     = "TCP"
	fwProtoUDP     = "UDP"
	fwProtoICMP    = "ICMP"
	fwProtoIPENCAP = "IPENCAP"
)

// firewallClient is the slice of *linodego.Client the managed-firewall code
// uses. Keeping it as an interface lets tests substitute a hand-rolled fake
// (per repo conventions — no codegen).
type firewallClient interface {
	ListFirewalls(ctx context.Context, opts *linodego.ListOptions) ([]linodego.Firewall, error)
	CreateFirewall(ctx context.Context, opts linodego.FirewallCreateOptions) (*linodego.Firewall, error)
	UpdateFirewallRules(ctx context.Context, firewallID int, rules linodego.FirewallRuleSet) (*linodego.FirewallRuleSet, error)
	DeleteFirewall(ctx context.Context, firewallID int) error
	ListFirewallDevices(ctx context.Context, firewallID int, opts *linodego.ListOptions) ([]linodego.FirewallDevice, error)
}

// firewallConfig is the provider_config.firewall sub-block.
type firewallConfig struct {
	// AllowInbound is a list of CIDRs OR sentinels (today: `auto`). At least
	// one entry that resolves to a non-empty set is required; this populates
	// the synthesized tcp/22 ACCEPT rule that lets the orchestrator SSH to
	// workers.
	AllowInbound []string `yaml:"allow_inbound"`

	// RefreshInterval is how often the background goroutine re-resolves the
	// sentinels and updates the firewall rules if they drift. Defaults to 1h,
	// minimum 1m enforced (so a typo can't melt the upstream services).
	RefreshInterval time.Duration `yaml:"refresh_interval"`

	// InboundPolicy / OutboundPolicy override the hardcoded defaults
	// (DROP / ACCEPT). Empty = use defaults. See #46.
	InboundPolicy  string `yaml:"inbound_policy"`
	OutboundPolicy string `yaml:"outbound_policy"`

	// ExtraInbound / ExtraOutbound are operator-supplied rules appended after
	// the synthesized tcp/22 ACCEPT rule (inbound) or used as the entire
	// outbound list. See #46. The combined rule count must stay within
	// Linode's 25-rule-per-firewall cap and each rule's addresses within the
	// 255-per-rule cap.
	ExtraInbound  []extraRule `yaml:"extra_inbound"`
	ExtraOutbound []extraRule `yaml:"extra_outbound"`
}

// extraRule mirrors the linodego.FirewallRule shape, expressed in the YAML
// surface fj-bellows operators interact with. Decoded directly from YAML and
// translated to linodego.FirewallRule by toLinodeRule.
//
// addresses is a single list of CIDRs (mixed v4/v6) — we bucket internally
// rather than making the operator track which family each entry is. The
// Linode API splits them into IPv4/IPv6 arrays under the hood; toLinodeRule
// does that.
type extraRule struct {
	Label       string   `yaml:"label"`
	Description string   `yaml:"description"`
	Action      string   `yaml:"action"`   // ACCEPT | DROP
	Protocol    string   `yaml:"protocol"` // TCP | UDP | ICMP | IPENCAP
	Ports       string   `yaml:"ports"`    // Linode-format port spec (e.g. "22", "8000-8100")
	Addresses   []string `yaml:"addresses"`
}

// validate checks the operator-supplied bits before any API calls fire.
// Catches policy/action/protocol typos and per-rule address cap overflow.
func (c firewallConfig) validate() error {
	if err := validatePolicy(c.InboundPolicy, "inbound_policy"); err != nil {
		return err
	}
	if err := validatePolicy(c.OutboundPolicy, "outbound_policy"); err != nil {
		return err
	}
	for i, r := range c.ExtraInbound {
		if err := r.validate(); err != nil {
			return fmt.Errorf("extra_inbound[%d]: %w", i, err)
		}
	}
	for i, r := range c.ExtraOutbound {
		if err := r.validate(); err != nil {
			return fmt.Errorf("extra_outbound[%d]: %w", i, err)
		}
	}
	return nil
}

func validatePolicy(p, field string) error {
	switch p {
	case "", fwActionAccept, fwInboundDrop:
		return nil
	default:
		return fmt.Errorf("%s: %q is not ACCEPT or DROP", field, p)
	}
}

var supportedExtraProtocols = map[string]bool{
	fwProtoTCP: true, fwProtoUDP: true, fwProtoICMP: true, fwProtoIPENCAP: true,
}

// validate is syntactic only — checks fields are well-formed and that
// addresses entries parse as CIDR or are recognised sentinels. Per-family
// 255-cap enforcement happens later in managedFirewall.buildExtraRule,
// after sentinels expand (auto can yield 1 or 2 entries depending on the
// host's network).
func (r extraRule) validate() error {
	if r.Label == "" {
		return errors.New("label is required")
	}
	switch r.Action {
	case fwActionAccept, fwInboundDrop:
	default:
		return fmt.Errorf("action %q is not ACCEPT or DROP", r.Action)
	}
	if !supportedExtraProtocols[r.Protocol] {
		return fmt.Errorf("protocol %q is not TCP/UDP/ICMP/IPENCAP", r.Protocol)
	}
	if len(r.Addresses) == 0 {
		return errors.New(`addresses is empty; a rule with no addresses matches nothing. ` +
			`Use [any] (or any-v4 / any-v6) for an unrestricted-source rule`)
	}
	for j, a := range r.Addresses {
		if isAddressSentinel(a) {
			continue
		}
		if _, _, err := net.ParseCIDR(a); err != nil {
			return fmt.Errorf("addresses[%d] %q is not a CIDR or a recognised sentinel (auto, any, any-v4, any-v6): %w", j, a, err)
		}
	}
	return nil
}

// (No standalone bucketExtraAddresses / toLinodeRule — those moved onto
// managedFirewall so they can reach the IP probe for the `auto` sentinel.
// See managedFirewall.resolveSentinels + buildExtraRule.)

// managedFirewall holds the runtime state for a single deployment's firewall:
// the resolved last-applied CIDR set, the firewall's Linode ID, the
// orchestration tag, the probes for sentinel resolution, and the logger for
// runtime warnings.
type managedFirewall struct {
	cfg     firewallConfig
	tag     string // cfg.Tag from the outer Linode provider
	client  firewallClient
	ipProbe externalIPProbe
	log     *slog.Logger

	// transportMode controls which ports the synthesized inbound ACCEPT
	// rules cover. Empty or "ssh" (legacy): tcp/22. "cache-gateway"
	// (FJB-54): udp/500, udp/4500, and ESP (IPENCAP) for the IPsec tunnel
	// that terminates on the cache nanode. Workers in cache-gateway mode
	// don't run an IPsec endpoint, but they share this firewall with the
	// cache so the rules are applied to both — the operator's LAN egress
	// can address IPsec ports on any instance the firewall is attached
	// to, but only the cache has a strongSwan listening; sends to worker
	// IPs land on closed UDP ports (kernel drops). Net effect: same
	// strict reduction in worker public attack surface either way.
	transportMode string

	// id and lastApplied are the in-process cache. id == 0 means the firewall
	// has been deleted by the cleanup path (next Provision lazy-recreates via
	// the refresh tick). lastApplied is the sorted CIDR set we last pushed;
	// the refresh tick only calls UpdateFirewallRules when the resolved set
	// differs.
	id          int
	lastApplied []string
}

// Transport modes. Kept as string constants here so the firewall code
// doesn't depend on internal/config (which would create a cycle once
// the orchestrator pulls in linode for tests). Values match
// config.Transport* exactly; callers (main.go) translate.
const (
	transportSSH          = ""              // legacy default — synthesized tcp/22 ACCEPT
	transportSSHExplicit  = "ssh"           // operator-explicit equivalent of ""
	transportCacheGateway = "cache-gateway" // FJB-54 — synthesized IPsec ACCEPT
)

// Sentinel tokens for the firewall config. allow_inbound supports `auto`;
// the operator's extra-rule addresses additionally support `any` / `any-v4`
// / `any-v6` shorthands so they don't have to spell out 0.0.0.0/0 + ::/0
// every time they want a wide-open rule.
const (
	sentinelAuto  = "auto"
	sentinelAny   = "any"
	sentinelAnyV4 = "any-v4"
	sentinelAnyV6 = "any-v6"
)

// isAddressSentinel reports whether s is one of the well-known tokens
// addresses-list fields accept (allow_inbound or extra rule .addresses).
// validate() uses this to accept the token syntactically; actual expansion
// happens later via managedFirewall.resolveSentinels which has the IP probe
// for `auto`.
func isAddressSentinel(s string) bool {
	switch s {
	case sentinelAuto, sentinelAny, sentinelAnyV4, sentinelAnyV6:
		return true
	}
	return false
}

// newManagedFirewall constructs the runtime helper. It validates the config
// (mutual exclusion with firewall_id is enforced by the caller); a zero
// RefreshInterval is normalised to 1h here. transportMode controls
// which ports the synthesized ACCEPT rules cover; empty == legacy ssh.
func newManagedFirewall(cfg firewallConfig, tag string, client firewallClient, log *slog.Logger, transportMode string) *managedFirewall {
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = time.Hour
	}
	if cfg.RefreshInterval < time.Minute {
		cfg.RefreshInterval = time.Minute
	}
	return &managedFirewall{
		cfg:           cfg,
		tag:           tag,
		client:        client,
		ipProbe:       defaultExternalIPProbe(),
		log:           log,
		transportMode: transportMode,
	}
}

// primeResolved resolves the sentinels once and caches the result on
// m.lastApplied. Called from Configure as the first step of standing up the
// managed firewall; ensureAtConfigure then uses the cached CIDRs to call
// CreateFirewall. The refresh goroutine re-resolves on its schedule for
// drift tracking, but Configure-time work happens exactly once.
func (m *managedFirewall) primeResolved(ctx context.Context) error {
	cidrs, err := m.resolveAllowInbound(ctx)
	if err != nil {
		return err
	}
	if len(cidrs) == 0 {
		return errors.New("managed firewall: allow_inbound resolved to zero CIDRs")
	}
	m.lastApplied = append([]string(nil), cidrs...)
	return nil
}

// ensure brings the firewall into existence on demand. No-op when the
// cached ID is still valid; otherwise re-runs ensureAtConfigure to
// recreate the firewall. The reaper sets m.id to 0 when it deletes the
// firewall on last-Destroy, so a subsequent Provision call needs this
// hook to self-heal instead of sending FirewallID=0 to Linode (which
// rejects it as "Firewall ID: 0 not found" — same shape as the
// placement-group bug in FJB-10). The PG check fires first in the
// cascade, but the FW path has the same shape.
func (m *managedFirewall) ensure(ctx context.Context) error {
	if m.id != 0 {
		return nil
	}
	m.log.Info("managed firewall: re-creating after teardown")
	return m.ensureAtConfigure(ctx)
}

// ensureAtConfigure creates/updates the firewall using the CIDRs already
// cached on m (resolved either at Configure time or by an earlier refresh).
// Failures here are returned to the caller; for the FIRST Provision the
// caller logs them and retries on the next tick. The retries hit only the
// Linode firewall API (not the sentinel sources), since the resolved CIDRs
// are cached — so a transient Linode-side error doesn't fan out into
// hammering GitHub or icanhazip.
func (m *managedFirewall) ensureAtConfigure(ctx context.Context) error {
	if len(m.lastApplied) == 0 {
		return errors.New("managed firewall: no CIDRs cached; Configure should have populated them")
	}
	ruleset, err := m.buildRuleSet(ctx, m.lastApplied)
	if err != nil {
		return err
	}
	id, err := m.ensureFirewall(ctx, ruleset)
	if err != nil {
		return err
	}
	m.id = id
	return nil
}

// startRefreshLoop spawns the drift-tracking goroutine. It runs with
// context.Background so it lives for the process; the daemon has no
// Provider.Shutdown hook so this is the lifecycle. Runtime failures are
// logged but never replace a working ruleset with an empty one.
func (m *managedFirewall) startRefreshLoop() {
	go m.refreshLoop()
}

func (m *managedFirewall) refreshLoop() {
	ticker := time.NewTicker(m.cfg.RefreshInterval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		m.refreshOnce(ctx)
		cancel()
	}
}

// refreshOnce re-resolves the sentinels and updates the firewall rules if the
// resulting set differs from the last-applied. Runtime failure semantics
// (deliberately different from Configure): log + keep the previous rules
// unchanged. A transient network blip during refresh must not punish a
// working deployment.
func (m *managedFirewall) refreshOnce(ctx context.Context) {
	cidrs, err := m.resolveAllowInbound(ctx)
	if err != nil {
		m.log.Warn("managed firewall: refresh sentinel resolution failed, keeping previous rules", "err", err)
		return
	}
	if len(cidrs) == 0 {
		m.log.Warn("managed firewall: refresh resolved to zero CIDRs, keeping previous rules")
		return
	}
	if reflect.DeepEqual(cidrs, m.lastApplied) {
		return
	}
	ruleset, err := m.buildRuleSet(ctx, cidrs)
	if err != nil {
		m.log.Warn("managed firewall: refresh buildRuleSet failed, keeping previous rules", "err", err)
		return
	}
	// If the firewall has been cleaned up (no instances remain) since we last
	// applied, ensureFirewall lazy-creates it. Otherwise it just updates rules.
	id, err := m.ensureFirewall(ctx, ruleset)
	if err != nil {
		m.log.Warn("managed firewall: refresh ensureFirewall failed, keeping previous rules", "err", err)
		return
	}
	m.id = id
	m.lastApplied = append([]string(nil), cidrs...)
	m.log.Info("managed firewall: rules updated from refresh", "cidrs", len(cidrs))
}

// resolveSentinels expands any/any-v4/any-v6/auto tokens to CIDR strings.
// Used by BOTH allow_inbound and the operator's extra-rule .addresses
// fields, so the sentinel vocabulary is consistent across surfaces.
// `auto` is the only one that touches the network (icanhazip via m.ipProbe);
// the rest are static. Unknown tokens that don't parse as CIDR error with
// a precise pointer including the index they came from.
func (m *managedFirewall) resolveSentinels(ctx context.Context, addrs []string) ([]string, error) {
	out := make([]string, 0, len(addrs))
	for j, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		switch a {
		case sentinelAuto:
			got, err := resolveExternalIP(ctx, m.ipProbe)
			if err != nil {
				return nil, fmt.Errorf("resolve %q: %w", sentinelAuto, err)
			}
			out = append(out, got...)
		case sentinelAny:
			out = append(out, "0.0.0.0/0", "::/0")
		case sentinelAnyV4:
			out = append(out, "0.0.0.0/0")
		case sentinelAnyV6:
			out = append(out, "::/0")
		default:
			if _, _, err := net.ParseCIDR(a); err != nil {
				return nil, fmt.Errorf("entry[%d] %q is neither a CIDR nor a recognised sentinel (auto, any, any-v4, any-v6)", j, a)
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// resolveAllowInbound resolves the allow_inbound entries (CIDRs + sentinels)
// into a deduped, sorted CIDR set. The synthesized tcp/22 rule(s) bucket
// these addresses across v4/v6 rules in buildRuleSet.
func (m *managedFirewall) resolveAllowInbound(ctx context.Context) ([]string, error) {
	raw, err := m.resolveSentinels(ctx, m.cfg.AllowInbound)
	if err != nil {
		return nil, fmt.Errorf("allow_inbound: %w", err)
	}
	seen := map[string]struct{}{}
	for _, c := range raw {
		seen[c] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// buildExtraRule resolves the operator's extra rule's addresses (sentinels +
// CIDRs), buckets them into v4/v6, and maps to a linodego.FirewallRule.
// Per-family 255-cap is enforced here, post-resolution.
func (m *managedFirewall) buildExtraRule(ctx context.Context, r extraRule) (linodego.FirewallRule, error) {
	resolved, err := m.resolveSentinels(ctx, r.Addresses)
	if err != nil {
		return linodego.FirewallRule{}, fmt.Errorf("extra rule %q addresses: %w", r.Label, err)
	}
	if len(resolved) == 0 {
		return linodego.FirewallRule{}, fmt.Errorf("extra rule %q resolved to zero addresses", r.Label)
	}
	v4, v6 := bucketCIDRs(resolved)
	if len(v4) > maxAddrsPerRule {
		return linodego.FirewallRule{}, fmt.Errorf("extra rule %q has %d v4 entries; Linode caps a rule at %d per family", r.Label, len(v4), maxAddrsPerRule)
	}
	if len(v6) > maxAddrsPerRule {
		return linodego.FirewallRule{}, fmt.Errorf("extra rule %q has %d v6 entries; Linode caps a rule at %d per family", r.Label, len(v6), maxAddrsPerRule)
	}
	if v4 == nil {
		v4 = []string{}
	}
	if v6 == nil {
		v6 = []string{}
	}
	return linodego.FirewallRule{
		Action:      r.Action,
		Label:       r.Label,
		Description: r.Description,
		Protocol:    linodego.NetworkProtocol(r.Protocol),
		Ports:       r.Ports,
		Addresses: linodego.NetworkAddresses{
			IPv4: &v4,
			IPv6: &v6,
		},
	}, nil
}

// Linode caps each firewall rule at 255 addresses per family and each
// firewall at 25 rules total. github-actions on its own can already exceed
// 255 v4 CIDRs, so we chunk the resolved set across multiple rules.
const (
	maxAddrsPerRule = 255
	maxRulesPerFW   = 25
)

// synthInboundSpec is one synthesized (protocol, ports, label-stem) tuple
// used to materialise an ACCEPT rule per address family from allow_inbound.
// Encodes the transport-mode-specific port surface in one place so
// buildRuleSet stays a generic chunker.
type synthInboundSpec struct {
	proto    linodego.NetworkProtocol
	ports    string // empty for protocols without ports (IPENCAP/ESP)
	labelTag string // suffix in "fj-bellows-<labelTag>-{v4,v6}-N"
	descNote string // human description fragment after "fj-bellows: "
}

// synthSpecsForTransport returns the list of (proto, ports) tuples each
// allow_inbound CIDR should ACCEPT, given the active transport mode.
// Empty / "ssh" (legacy default) keeps tcp/22; "cache-gateway" switches
// to the IPsec port set so the cache nanode's strongSwan can receive
// the tunnel from the LAN side.
func synthSpecsForTransport(mode string) []synthInboundSpec {
	switch mode {
	case transportCacheGateway:
		return []synthInboundSpec{
			{proto: linodego.UDP, ports: "500", labelTag: "ipsec-ike", descNote: "udp/500 (IKE) from allow_inbound"},
			{proto: linodego.UDP, ports: "4500", labelTag: "ipsec-natt", descNote: "udp/4500 (IPsec NAT-T) from allow_inbound"},
			{proto: linodego.IPENCAP, ports: "", labelTag: "ipsec-esp", descNote: "ESP from allow_inbound"},
		}
	default: // transportSSH, transportSSHExplicit, or any unrecognised mode
		return []synthInboundSpec{
			{proto: linodego.TCP, ports: "22", labelTag: "ssh", descNote: "tcp/22 from allow_inbound"},
		}
	}
}

// buildRuleSet renders a CIDR set into the firewall ruleset Linode expects.
// The synthesized inbound rule(s) ACCEPT specific (proto, port) tuples from
// those CIDRs — the tuples come from synthSpecsForTransport, which switches
// between SSH (legacy) and IPsec (cache-gateway) port surfaces. Default
// policy is deny-inbound + accept-outbound, both overridable via
// firewallConfig.{Inbound,Outbound}Policy. Operator-supplied extras
// (firewallConfig.ExtraInbound / .ExtraOutbound) are appended in the order
// given.
//
// Linode caps each rule at 255 addresses TOTAL (v4+v6 combined; the API
// error `[rules.inbound[0].addresses] Too many addresses submitted. Max
// allowed is 255` was observed for a rule mixing 255 v4 + a handful of v6).
// To avoid the totalling trap we split families into separate rules
// entirely — every emitted rule is either v4-only or v6-only, capped at 255
// entries.
//
// Returns an error if the resulting rule count would exceed Linode's
// 25-rule-per-firewall cap, or if any extra rule's addresses resolve outside
// Linode's per-family caps.
func (m *managedFirewall) buildRuleSet(ctx context.Context, cidrs []string) (linodego.FirewallRuleSet, error) {
	cfg := m.cfg
	v4, v6 := bucketCIDRs(cidrs)
	v4Chunks := chunkAddrs(v4, maxAddrsPerRule)
	v6Chunks := chunkAddrs(v6, maxAddrsPerRule)
	specs := synthSpecsForTransport(m.transportMode)
	// One spec yields up to (v4Chunks + v6Chunks) rules; sum across specs.
	chunkRules := len(v4Chunks) + len(v6Chunks)
	synthCount := len(specs) * chunkRules

	inboundPolicy := cfg.InboundPolicy
	if inboundPolicy == "" {
		inboundPolicy = fwInboundDrop
	}
	outboundPolicy := cfg.OutboundPolicy
	if outboundPolicy == "" {
		outboundPolicy = fwOutboundAccept
	}

	// Pre-flight rule-count check: synth (specs × chunks from allow_inbound) +
	// extras must fit under Linode's 25-rule-per-firewall cap. Done before
	// any allocation so we error early with a useful message.
	totalInbound := synthCount + len(cfg.ExtraInbound)
	if synthCount == 0 {
		// Degenerate (empty allow_inbound) still emits one empty rule per
		// spec so the firewall create succeeds. Orchestrator already
		// errors at Configure if allow_inbound resolves to zero; this
		// branch only fires in tests + extras-only configs.
		totalInbound = len(specs) + len(cfg.ExtraInbound)
	}
	if totalInbound > maxRulesPerFW {
		return linodego.FirewallRuleSet{}, fmt.Errorf(
			"firewall: inbound rule count (%d synth + %d extra = %d) exceeds Linode's per-firewall cap of %d",
			max(synthCount, len(specs)), len(cfg.ExtraInbound), totalInbound, maxRulesPerFW,
		)
	}
	if len(cfg.ExtraOutbound) > maxRulesPerFW {
		return linodego.FirewallRuleSet{}, fmt.Errorf(
			"firewall: extra_outbound rule count (%d) exceeds Linode's per-firewall cap of %d",
			len(cfg.ExtraOutbound), maxRulesPerFW,
		)
	}

	inbound := make([]linodego.FirewallRule, 0, totalInbound)
	empty := []string{}
	if synthCount == 0 {
		// One empty placeholder per spec so the labels remain unique.
		for _, s := range specs {
			inbound = append(inbound, linodego.FirewallRule{
				Action:    fwActionAccept,
				Label:     fmt.Sprintf("fj-bellows-%s-v4-1", s.labelTag),
				Protocol:  s.proto,
				Ports:     s.ports,
				Addresses: linodego.NetworkAddresses{IPv4: &empty, IPv6: &empty},
			})
		}
	}
	for _, s := range specs {
		for i, chunk := range v4Chunks {
			c := chunk
			inbound = append(inbound, linodego.FirewallRule{
				Action:      fwActionAccept,
				Label:       fmt.Sprintf("fj-bellows-%s-v4-%d", s.labelTag, i+1),
				Description: "fj-bellows: " + s.descNote + " (v4)",
				Protocol:    s.proto,
				Ports:       s.ports,
				Addresses:   linodego.NetworkAddresses{IPv4: &c, IPv6: &empty},
			})
		}
		for i, chunk := range v6Chunks {
			c := chunk
			inbound = append(inbound, linodego.FirewallRule{
				Action:      fwActionAccept,
				Label:       fmt.Sprintf("fj-bellows-%s-v6-%d", s.labelTag, i+1),
				Description: "fj-bellows: " + s.descNote + " (v6)",
				Protocol:    s.proto,
				Ports:       s.ports,
				Addresses:   linodego.NetworkAddresses{IPv4: &empty, IPv6: &c},
			})
		}
	}
	for _, r := range cfg.ExtraInbound {
		built, err := m.buildExtraRule(ctx, r)
		if err != nil {
			return linodego.FirewallRuleSet{}, err
		}
		inbound = append(inbound, built)
	}

	outbound := make([]linodego.FirewallRule, 0, len(cfg.ExtraOutbound))
	for _, r := range cfg.ExtraOutbound {
		built, err := m.buildExtraRule(ctx, r)
		if err != nil {
			return linodego.FirewallRuleSet{}, err
		}
		outbound = append(outbound, built)
	}

	return linodego.FirewallRuleSet{
		Inbound:        inbound,
		InboundPolicy:  inboundPolicy,
		Outbound:       outbound,
		OutboundPolicy: outboundPolicy,
	}, nil
}

// chunkAddrs splits xs into slices of at most n each. Returns nil for an
// empty input so callers can distinguish "no v6 at all" from "one empty
// chunk of v6".
func chunkAddrs(xs []string, n int) [][]string {
	if len(xs) == 0 {
		return nil
	}
	out := make([][]string, 0, (len(xs)+n-1)/n)
	for i := 0; i < len(xs); i += n {
		end := min(i+n, len(xs))
		out = append(out, xs[i:end])
	}
	return out
}

// bucketCIDRs partitions a sorted CIDR slice into v4 and v6 buckets.
func bucketCIDRs(cidrs []string) (v4, v6 []string) {
	for _, c := range cidrs {
		ip, _, err := net.ParseCIDR(c)
		if err != nil {
			continue // shouldn't happen post-resolveAllowInbound, defensive
		}
		if ip.To4() != nil {
			v4 = append(v4, c)
		} else {
			v6 = append(v6, c)
		}
	}
	return v4, v6
}

// ensureFirewall finds-or-creates the firewall tagged with our tag, applying
// the given ruleset. On update, only calls UpdateFirewallRules when the rules
// drift (semantically; we compare the input addresses to the firewall's
// current rule).
func (m *managedFirewall) ensureFirewall(ctx context.Context, ruleset linodego.FirewallRuleSet) (int, error) {
	existing, err := m.findFirewall(ctx)
	if err != nil {
		return 0, fmt.Errorf("find firewall: %w", err)
	}
	if existing == nil {
		created, err := m.client.CreateFirewall(ctx, linodego.FirewallCreateOptions{
			Label: firewallLabel(m.tag),
			Tags:  []string{m.tag},
			Rules: ruleset,
		})
		if err != nil {
			return 0, fmt.Errorf("create firewall: %w", err)
		}
		return created.ID, nil
	}
	// Compare addresses semantically. Linode round-trips InboundPolicy/etc
	// unchanged, so the addresses field is what actually varies.
	if !ruleSetAddrsEqual(existing.Rules, ruleset) {
		if _, err := m.client.UpdateFirewallRules(ctx, existing.ID, ruleset); err != nil {
			return existing.ID, fmt.Errorf("update firewall rules: %w", err)
		}
	}
	return existing.ID, nil
}

// findFirewall returns the firewall tagged with our cfg.Tag, or nil if none
// exists. Matches by tag rather than label: labels are truncated to fit
// Linode's 32-char cap and might collide across long deployment tags, but the
// tag field is the canonical ownership marker (same as Linode instances).
func (m *managedFirewall) findFirewall(ctx context.Context) (*linodego.Firewall, error) {
	fws, err := m.client.ListFirewalls(ctx, nil)
	if err != nil {
		return nil, err
	}
	for i := range fws {
		if slices.Contains(fws[i].Tags, m.tag) {
			return &fws[i], nil
		}
	}
	return nil, nil
}

// maybeCleanupFirewall deletes the firewall iff (a) it exists, (b) we own it
// (tag match), and (c) it has no devices attached. Called from Destroy after
// the underlying DeleteInstance returns — the last instance going away is what
// drives cleanup. -destroy-on-exit follows the same code path (per-instance
// Destroys), so it's covered without needing a Provider.Shutdown hook.
func (m *managedFirewall) maybeCleanupFirewall(ctx context.Context) {
	fw, err := m.findFirewall(ctx)
	if err != nil {
		m.log.Warn("managed firewall: lookup during cleanup", "err", err)
		return
	}
	if fw == nil {
		return
	}
	devs, err := m.client.ListFirewallDevices(ctx, fw.ID, nil)
	if err != nil {
		m.log.Warn("managed firewall: list devices during cleanup", "err", err)
		return
	}
	if len(devs) > 0 {
		return
	}
	if err := m.client.DeleteFirewall(ctx, fw.ID); err != nil {
		m.log.Warn("managed firewall: delete during cleanup", "id", fw.ID, "err", err)
		return
	}
	m.id = 0
	m.log.Info("managed firewall: deleted (no devices remained)", "id", fw.ID)
}

// firewallLabel renders cfg.Tag into a string that fits Linode's firewall
// label rules: 3-32 chars from [A-Za-z0-9_.-].
func firewallLabel(tag string) string {
	const firewallLabelMin = 3
	const firewallLabelMax = 32
	return sanitizeLabel("fj-bellows-", tag, firewallLabelMin, firewallLabelMax)
}

// sanitizeLabel renders a deployment tag into a Linode label that fits a
// given min/max character budget. Invalid characters are replaced with '-';
// long candidates are truncated with a SHA-256-derived suffix so two
// distinct long tags can't accidentally collide. Used by both
// firewallLabel (cap 32) and placementGroupLabel (cap 64).
func sanitizeLabel(prefix, tag string, minLen, maxLen int) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, tag)
	candidate := prefix + clean
	if len(candidate) <= maxLen {
		for len(candidate) < minLen {
			candidate += "-x"
		}
		return candidate
	}
	// SHA-256 is overkill for a label-collision suffix, but avoids the
	// linter's blocklist on weaker primitives and the cost is trivial at
	// startup.
	sum := sha256.Sum256([]byte(tag))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	head := candidate[:maxLen-len(suffix)]
	return head + suffix
}

// ruleSetAddrsEqual reports whether two rulesets have the same inbound v4/v6
// allow lists, treating the rule-chunking as an implementation detail
// (collapses all v4 across all inbound rules into one sorted set, same for
// v6, then compares). We accept that an Outbound or InboundPolicy difference
// would not be picked up — but we never change those, so this is the right
// granularity for drift detection.
func ruleSetAddrsEqual(a, b linodego.FirewallRuleSet) bool {
	addrs := func(rs linodego.FirewallRuleSet) (v4, v6 []string) {
		for _, r := range rs.Inbound {
			if r.Addresses.IPv4 != nil {
				v4 = append(v4, *r.Addresses.IPv4...)
			}
			if r.Addresses.IPv6 != nil {
				v6 = append(v6, *r.Addresses.IPv6...)
			}
		}
		sort.Strings(v4)
		sort.Strings(v6)
		return v4, v6
	}
	av4, av6 := addrs(a)
	bv4, bv6 := addrs(b)
	return slices.Equal(av4, bv4) && slices.Equal(av6, bv6)
}
