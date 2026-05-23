package linode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	cfg       firewallConfig
	tag       string // cfg.Tag from the outer Linode provider
	client    firewallClient
	ipProbe   externalIPProbe
	ghMetaURL string
	httpDoer  httpDoer
	log       *slog.Logger

	// id and lastApplied are the in-process cache. id == 0 means the firewall
	// either has not been created yet, or was deleted by the cleanup path
	// (next Provision will lazy-create it). lastApplied is the sorted CIDR
	// set we last pushed; the refresh tick only calls UpdateFirewallRules
	// when the resolved set differs.
	id          int
	lastApplied []string
}

// supportedSentinelSchemes is the set of well-known tokens allow_inbound
// accepts in place of a CIDR. Anything else that doesn't parse as CIDR is a
// hard error at Configure time so a typo (`gha`, `github_actions`) fails
// loudly instead of silently disabling the protection.
const (
	sentinelAuto          = "auto"
	sentinelGithubActions = "github-actions"
)

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
		cfg:       cfg,
		tag:       tag,
		client:    client,
		ipProbe:   defaultExternalIPProbe(),
		ghMetaURL: defaultGithubMetaURL,
		httpDoer:  &http.Client{Timeout: 5 * time.Second},
		log:       log,
	}
}

// ensureAtConfigure resolves the allow-list and creates/updates the firewall
// at orchestrator startup. Failures here are FATAL — Configure returns the
// error, the daemon refuses to start. Better than provisioning workers
// nobody can reach. See #26.
func (m *managedFirewall) ensureAtConfigure(ctx context.Context) error {
	cidrs, err := m.resolveAllowInbound(ctx)
	if err != nil {
		return err
	}
	if len(cidrs) == 0 {
		return errors.New("managed firewall: allow_inbound resolved to zero CIDRs; refusing to start with a default-deny firewall nobody can reach")
	}
	id, err := m.ensureFirewall(ctx, buildRuleSet(cidrs))
	if err != nil {
		return err
	}
	m.id = id
	m.lastApplied = append([]string(nil), cidrs...)
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
	// If the firewall has been cleaned up (no instances remain) since we last
	// applied, ensureFirewall lazy-creates it. Otherwise it just updates rules.
	id, err := m.ensureFirewall(ctx, buildRuleSet(cidrs))
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
		case sentinelGithubActions:
			got, err := fetchGithubActionsCIDRs(ctx, m.httpDoer, m.ghMetaURL)
			if err != nil {
				return nil, fmt.Errorf("resolve %q: %w", sentinelGithubActions, err)
			}
			for _, c := range got {
				add(c)
			}
		default:
			if _, _, err := net.ParseCIDR(entry); err != nil {
				return nil, fmt.Errorf("allow_inbound entry %q is neither a CIDR nor a recognised sentinel (auto, github-actions)", entry)
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

// buildRuleSet renders a CIDR set into a default-deny inbound firewall ruleset
// that accepts only tcp/22 from those CIDRs. Outbound is unrestricted (workers
// need HTTPS to Forgejo and registries). The CIDRs are bucketed into v4/v6
// because Linode's API requires separate arrays.
func buildRuleSet(cidrs []string) linodego.FirewallRuleSet {
	v4, v6 := bucketCIDRs(cidrs)
	rule := linodego.FirewallRule{
		Action:      fwActionAccept,
		Label:       "fj-bellows-ssh",
		Description: "fj-bellows: tcp/22 from configured allow_inbound",
		Protocol:    linodego.TCP,
		Ports:       "22",
		Addresses: linodego.NetworkAddresses{
			IPv4: &v4,
			IPv6: &v6,
		},
	}
	return linodego.FirewallRuleSet{
		Inbound:        []linodego.FirewallRule{rule},
		InboundPolicy:  fwInboundDrop,
		Outbound:       []linodego.FirewallRule{},
		OutboundPolicy: fwOutboundAccept,
	}
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
// allow lists. We accept that an Outbound difference, InboundPolicy change,
// etc. would not be picked up — but we never change those fields, so this is
// the right granularity for drift detection.
func ruleSetAddrsEqual(a, b linodego.FirewallRuleSet) bool {
	addrs := func(rs linodego.FirewallRuleSet) (v4, v6 []string) {
		if len(rs.Inbound) == 0 {
			return nil, nil
		}
		if rs.Inbound[0].Addresses.IPv4 != nil {
			v4 = append([]string(nil), *rs.Inbound[0].Addresses.IPv4...)
		}
		if rs.Inbound[0].Addresses.IPv6 != nil {
			v6 = append([]string(nil), *rs.Inbound[0].Addresses.IPv6...)
		}
		sort.Strings(v4)
		sort.Strings(v6)
		return v4, v6
	}
	av4, av6 := addrs(a)
	bv4, bv6 := addrs(b)
	return slices.Equal(av4, bv4) && slices.Equal(av6, bv6)
}
