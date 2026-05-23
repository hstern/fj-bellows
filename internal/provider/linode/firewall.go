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

// fwActionAccept / fwInboundDrop / fwOutboundAccept centralise the Linode
// firewall action strings — repeated across the rule + tests, lint catches
// them otherwise.
const (
	fwActionAccept   = "ACCEPT"
	fwInboundDrop    = "DROP"
	fwOutboundAccept = "ACCEPT"
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
	// AllowInbound is a list of CIDRs OR sentinels (`auto`, `github-actions`).
	// At least one entry that resolves to a non-empty set is required.
	AllowInbound []string `yaml:"allow_inbound"`

	// RefreshInterval is how often the background goroutine re-resolves the
	// sentinels and updates the firewall rules if they drift. Defaults to 1h,
	// minimum 1m enforced (so a typo can't melt the upstream services).
	RefreshInterval time.Duration `yaml:"refresh_interval"`
}

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

	// id and lastApplied are the in-process cache. id == 0 means the firewall
	// has been deleted by the cleanup path (next Provision lazy-recreates via
	// the refresh tick). lastApplied is the sorted CIDR set we last pushed;
	// the refresh tick only calls UpdateFirewallRules when the resolved set
	// differs.
	id          int
	lastApplied []string
}

// supportedSentinelSchemes is the set of well-known tokens allow_inbound
// accepts in place of a CIDR. Anything else that doesn't parse as CIDR is a
// hard error at Configure time so a typo (`auto-detect`, `myip`) fails
// loudly instead of silently disabling the protection.
const sentinelAuto = "auto"

// newManagedFirewall constructs the runtime helper. It validates the config
// (mutual exclusion with firewall_id is enforced by the caller); a zero
// RefreshInterval is normalised to 1h here.
func newManagedFirewall(cfg firewallConfig, tag string, client firewallClient, log *slog.Logger) *managedFirewall {
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = time.Hour
	}
	if cfg.RefreshInterval < time.Minute {
		cfg.RefreshInterval = time.Minute
	}
	return &managedFirewall{
		cfg:     cfg,
		tag:     tag,
		client:  client,
		ipProbe: defaultExternalIPProbe(),
		log:     log,
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
	ruleset, err := buildRuleSet(m.lastApplied)
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
	ruleset, err := buildRuleSet(cidrs)
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

// resolveAllowInbound walks the allow_inbound entries and expands sentinels
// into their CIDR sets. Order doesn't matter (the result is sorted+deduped).
// Returns an error if any sentinel fetch fails or any entry is neither a
// CIDR nor a recognised sentinel.
func (m *managedFirewall) resolveAllowInbound(ctx context.Context) ([]string, error) {
	seen := map[string]struct{}{}
	add := func(cidr string) {
		if _, ok := seen[cidr]; !ok {
			seen[cidr] = struct{}{}
		}
	}
	for _, entry := range m.cfg.AllowInbound {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		switch entry {
		case sentinelAuto:
			got, err := resolveExternalIP(ctx, m.ipProbe)
			if err != nil {
				return nil, fmt.Errorf("resolve %q: %w", sentinelAuto, err)
			}
			for _, c := range got {
				add(c)
			}
		default:
			if _, _, err := net.ParseCIDR(entry); err != nil {
				return nil, fmt.Errorf("allow_inbound entry %q is neither a CIDR nor a recognised sentinel (auto)", entry)
			}
			add(entry)
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// Linode caps each firewall rule at 255 addresses per family and each
// firewall at 25 rules total. github-actions on its own can already exceed
// 255 v4 CIDRs, so we chunk the resolved set across multiple rules.
const (
	maxAddrsPerRule = 255
	maxRulesPerFW   = 25
)

// buildRuleSet renders a CIDR set into a default-deny inbound firewall
// ruleset that accepts only tcp/22 from those CIDRs. Outbound is unrestricted
// (workers need HTTPS to Forgejo and registries).
//
// Linode caps each rule at 255 addresses TOTAL (v4+v6 combined; the API error
// `[rules.inbound[0].addresses] Too many addresses submitted. Max allowed
// is 255` was observed for a rule mixing 255 v4 + a handful of v6). To avoid
// the totalling trap we split families into separate rules entirely — every
// emitted rule is either v4-only or v6-only, capped at 255 entries.
//
// Returns an error if the resulting rule count would exceed Linode's
// 25-rule-per-firewall cap.
func buildRuleSet(cidrs []string) (linodego.FirewallRuleSet, error) {
	v4, v6 := bucketCIDRs(cidrs)
	v4Chunks := chunkAddrs(v4, maxAddrsPerRule)
	v6Chunks := chunkAddrs(v6, maxAddrsPerRule)
	nRules := len(v4Chunks) + len(v6Chunks)
	if nRules == 0 {
		// Degenerate: caller built a ruleset from an empty CIDR list. Emit
		// a single empty rule so the firewall create succeeds (the
		// orchestrator already errored at Configure if allow_inbound was
		// empty post-resolve; this branch only fires in tests).
		empty := []string{}
		return linodego.FirewallRuleSet{
			Inbound: []linodego.FirewallRule{{
				Action:    fwActionAccept,
				Label:     "fj-bellows-ssh-v4-1",
				Protocol:  linodego.TCP,
				Ports:     "22",
				Addresses: linodego.NetworkAddresses{IPv4: &empty, IPv6: &empty},
			}},
			InboundPolicy:  fwInboundDrop,
			Outbound:       []linodego.FirewallRule{},
			OutboundPolicy: fwOutboundAccept,
		}, nil
	}
	if nRules > maxRulesPerFW {
		return linodego.FirewallRuleSet{}, fmt.Errorf(
			"firewall: allow_inbound resolves to %d v4 + %d v6 CIDRs, which needs %d rules (255/family/rule) but Linode caps a firewall at %d",
			len(v4), len(v6), nRules, maxRulesPerFW,
		)
	}
	rules := make([]linodego.FirewallRule, 0, nRules)
	empty := []string{}
	for i, chunk := range v4Chunks {
		c := chunk // capture
		rules = append(rules, linodego.FirewallRule{
			Action:      fwActionAccept,
			Label:       fmt.Sprintf("fj-bellows-ssh-v4-%d", i+1),
			Description: "fj-bellows: tcp/22 from allow_inbound (v4)",
			Protocol:    linodego.TCP,
			Ports:       "22",
			Addresses:   linodego.NetworkAddresses{IPv4: &c, IPv6: &empty},
		})
	}
	for i, chunk := range v6Chunks {
		c := chunk
		rules = append(rules, linodego.FirewallRule{
			Action:      fwActionAccept,
			Label:       fmt.Sprintf("fj-bellows-ssh-v6-%d", i+1),
			Description: "fj-bellows: tcp/22 from allow_inbound (v6)",
			Protocol:    linodego.TCP,
			Ports:       "22",
			Addresses:   linodego.NetworkAddresses{IPv4: &empty, IPv6: &c},
		})
	}
	return linodego.FirewallRuleSet{
		Inbound:        rules,
		InboundPolicy:  fwInboundDrop,
		Outbound:       []linodego.FirewallRule{},
		OutboundPolicy: fwOutboundAccept,
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
// label rules: 3-32 chars from [A-Za-z0-9_.-]. Long or invalid-character
// tags are sanitized and truncated with a short hash suffix to keep
// uniqueness across truncation collisions.
func firewallLabel(tag string) string {
	const prefix = "fj-bellows-"
	const maxLen = 32

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
		// Pad short candidates to the 3-char minimum if needed.
		if len(candidate) < 3 {
			candidate += "-x"
		}
		return candidate
	}
	// Hash the tag and use the first 8 hex chars as a suffix so two
	// distinct long tags can't accidentally collide on the truncated label.
	// SHA-256 is overkill for collision avoidance on a label suffix, but it
	// avoids the linter's blocklist on weaker primitives and the cost is
	// trivial for a startup-time call.
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
