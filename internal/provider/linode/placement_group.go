package linode

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/linode/linodego"
)

// placementGroupClient is the slice of *linodego.Client the managed-PG code
// uses. Hand-rolled fake satisfies it in tests (per repo conventions — no
// codegen).
type placementGroupClient interface {
	ListPlacementGroups(ctx context.Context, opts *linodego.ListOptions) ([]linodego.PlacementGroup, error)
	GetPlacementGroup(ctx context.Context, id int) (*linodego.PlacementGroup, error)
	CreatePlacementGroup(ctx context.Context, opts linodego.PlacementGroupCreateOptions) (*linodego.PlacementGroup, error)
	DeletePlacementGroup(ctx context.Context, id int) error
}

// placementGroupConfig is the provider_config.placement_group sub-block.
type placementGroupConfig struct {
	// Enforcement is "flexible" (default) or "strict". flexible lets Linode
	// fall back to colocation when no compliant slot is available; strict
	// refuses the create instead (Provision will error). The affinity TYPE
	// is implicit at anti_affinity:local — the only value the Linode API
	// accepts today; surfacing it as a knob would be cosmetic.
	Enforcement string `yaml:"enforcement"`
}

// validate is syntactic — checks enforcement is a known token. No API calls.
func (c placementGroupConfig) validate() error {
	switch c.Enforcement {
	case "", string(linodego.PlacementGroupPolicyFlexible), string(linodego.PlacementGroupPolicyStrict):
		return nil
	default:
		return fmt.Errorf("placement_group.enforcement: %q is not %q or %q",
			c.Enforcement,
			linodego.PlacementGroupPolicyFlexible,
			linodego.PlacementGroupPolicyStrict)
	}
}

// resolvedPolicy returns the linodego policy with the default ("flexible")
// substituted when the operator left it blank.
func (c placementGroupConfig) resolvedPolicy() linodego.PlacementGroupPolicy {
	if c.Enforcement == "" {
		return linodego.PlacementGroupPolicyFlexible
	}
	return linodego.PlacementGroupPolicy(c.Enforcement)
}

// managedPlacementGroup holds the runtime state for a single deployment's
// placement group: the cached ID and the orchestration tag (which drives
// the label used for ownership).
type managedPlacementGroup struct {
	cfg    placementGroupConfig
	tag    string
	region string
	client placementGroupClient
	log    *slog.Logger

	// id is the Linode placement-group ID. Zero means the group either
	// hasn't been created yet, or was deleted by the cleanup path (next
	// Provision would lazy-recreate via ensureAtConfigure on next startup).
	id int
}

func newManagedPlacementGroup(cfg placementGroupConfig, tag, region string, client placementGroupClient, log *slog.Logger) *managedPlacementGroup {
	return &managedPlacementGroup{
		cfg:    cfg,
		tag:    tag,
		region: region,
		client: client,
		log:    log,
	}
}

// ensure brings the placement group into existence on demand. No-op
// when the cached ID is still valid; otherwise re-runs ensureAtConfigure
// to recreate the group. The reaper sets m.id to 0 when it deletes the
// group on last-Destroy, so a subsequent Provision call needs this hook
// to self-heal instead of sending PlacementGroup.ID=0 to Linode (which
// rejects it as "Placement Group ID: 0 not found", wedging the
// orchestrator in a retry loop — see FJB-10).
func (m *managedPlacementGroup) ensure(ctx context.Context) error {
	if m.id != 0 {
		return nil
	}
	m.log.Info("managed placement group: re-creating after teardown")
	return m.ensureAtConfigure(ctx)
}

// ensureAtConfigure finds-or-creates the placement group at Configure time.
// Failures here are returned to the caller; Configure then refuses to start
// the daemon (eager-create matches the managed firewall design — surface
// PAT-scope mistakes immediately).
func (m *managedPlacementGroup) ensureAtConfigure(ctx context.Context) error {
	existing, err := m.findGroup(ctx)
	if err != nil {
		return fmt.Errorf("find placement group: %w", err)
	}
	if existing != nil {
		m.id = existing.ID
		return nil
	}
	created, err := m.client.CreatePlacementGroup(ctx, linodego.PlacementGroupCreateOptions{
		Label:                placementGroupLabel(m.tag),
		Region:               m.region,
		PlacementGroupType:   linodego.PlacementGroupTypeAntiAffinityLocal,
		PlacementGroupPolicy: m.cfg.resolvedPolicy(),
	})
	if err != nil {
		return fmt.Errorf("create placement group: %w", err)
	}
	m.id = created.ID
	return nil
}

// findGroup returns the placement group whose label matches our deployment,
// or nil if none. Placement groups have no Tags field (unlike Firewalls);
// ownership is by label alone.
func (m *managedPlacementGroup) findGroup(ctx context.Context) (*linodego.PlacementGroup, error) {
	want := placementGroupLabel(m.tag)
	pgs, err := m.client.ListPlacementGroups(ctx, nil)
	if err != nil {
		return nil, err
	}
	for i := range pgs {
		if pgs[i].Label == want {
			return &pgs[i], nil
		}
	}
	return nil, nil
}

// maybeCleanupPlacementGroup deletes the placement group iff it exists and
// has no members left. Called from Linode.Destroy after the underlying
// DeleteInstance succeeds — Linode auto-unassigns the destroyed Linode from
// the group, so the last Destroy in a deployment naturally drives cleanup.
// -destroy-on-exit follows the same code path (per-instance Destroys), so
// the orchestrator's normal shutdown reclaims the group for free.
func (m *managedPlacementGroup) maybeCleanupPlacementGroup(ctx context.Context) {
	pg, err := m.findGroup(ctx)
	if err != nil {
		m.log.Warn("managed placement group: lookup during cleanup", "err", err)
		return
	}
	if pg == nil {
		return
	}
	// GetPlacementGroup returns the full member list; List might not
	// (depending on the API version). Re-fetch to be sure.
	full, err := m.client.GetPlacementGroup(ctx, pg.ID)
	if err != nil {
		m.log.Warn("managed placement group: get during cleanup", "id", pg.ID, "err", err)
		return
	}
	if len(full.Members) > 0 {
		return
	}
	if err := m.client.DeletePlacementGroup(ctx, full.ID); err != nil {
		m.log.Warn("managed placement group: delete during cleanup", "id", full.ID, "err", err)
		return
	}
	m.id = 0
	m.log.Info("managed placement group: deleted (no members remained)", "id", full.ID)
}

// placementGroupLabel renders cfg.Tag into a string that fits Linode's
// placement-group label rules: 1-64 chars from [A-Za-z0-9_.-]. Mirror of
// firewallLabel; both delegate to sanitizeLabel, just with a wider char
// budget for PGs (64 vs 32).
func placementGroupLabel(tag string) string {
	const pgLabelMin = 1
	const pgLabelMax = 64
	return sanitizeLabel("fj-bellows-", tag, pgLabelMin, pgLabelMax)
}

// linodego.Client must satisfy our reduced interface; compile-time guard.
var _ placementGroupClient = (*linodego.Client)(nil)
