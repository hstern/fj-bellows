package linode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"

	"github.com/linode/linodego"
)

// vpcClient is the slice of *linodego.Client the managed-VPC code uses.
// Hand-rolled fake satisfies it in tests (per repo conventions — no codegen).
type vpcClient interface {
	ListVPCs(ctx context.Context, opts *linodego.ListOptions) ([]linodego.VPC, error)
	GetVPC(ctx context.Context, id int) (*linodego.VPC, error)
	CreateVPC(ctx context.Context, opts linodego.VPCCreateOptions) (*linodego.VPC, error)
	DeleteVPC(ctx context.Context, id int) error
	CreateVPCSubnet(ctx context.Context, opts linodego.VPCSubnetCreateOptions, vpcID int) (*linodego.VPCSubnet, error)
	DeleteVPCSubnet(ctx context.Context, vpcID, subnetID int) error
}

// vpcConfig is the provider_config.vpc sub-block.
type vpcConfig struct {
	// Name overrides the deployment-derived VPC label. Empty = derive from
	// cfg.Tag via vpcLabel.
	Name string `yaml:"name"`

	// Subnets keyed by name. At least one is required; workers attach to
	// WorkerSubnet (or the first alphabetically when unset).
	Subnets map[string]subnetConfig `yaml:"subnets"`

	// WorkerSubnet selects the subnet workers attach their VPC NIC to.
	// Defaults to the alphabetically-first key in Subnets — deterministic
	// so multi-process Provision calls don't fight, and obvious when only
	// one subnet is declared.
	WorkerSubnet string `yaml:"worker_subnet"`
}

// subnetConfig is one entry under vpcConfig.Subnets.
type subnetConfig struct {
	IPv4 string `yaml:"ipv4"`
}

// validate is syntactic — CIDR parses, at least one subnet, worker_subnet
// (if set) names a declared subnet. No API calls.
func (c vpcConfig) validate() error {
	if len(c.Subnets) == 0 {
		return errors.New("vpc: subnets is required (declare at least one)")
	}
	for name, s := range c.Subnets {
		if name == "" {
			return errors.New("vpc: subnet name must be non-empty")
		}
		if s.IPv4 == "" {
			return fmt.Errorf("vpc: subnets[%q]: ipv4 is required", name)
		}
		if _, _, err := net.ParseCIDR(s.IPv4); err != nil {
			return fmt.Errorf("vpc: subnets[%q]: ipv4 %q is not a CIDR: %w", name, s.IPv4, err)
		}
	}
	if c.WorkerSubnet != "" {
		if _, ok := c.Subnets[c.WorkerSubnet]; !ok {
			return fmt.Errorf("vpc: worker_subnet %q is not declared under subnets", c.WorkerSubnet)
		}
	}
	return nil
}

// resolvedWorkerSubnet returns the subnet name workers attach to.
// alphabetical-first default keeps the choice deterministic across
// concurrent Provision callers without requiring a config knob in the
// single-subnet case.
func (c vpcConfig) resolvedWorkerSubnet() string {
	if c.WorkerSubnet != "" {
		return c.WorkerSubnet
	}
	names := sortedSubnetNames(c.Subnets)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// managedVPC holds runtime state for a single deployment's VPC: the
// VPC ID and the name→subnet-ID map. id == 0 means the VPC has been
// deleted by the cleanup path (next Provision lazy-recreates).
type managedVPC struct {
	cfg    vpcConfig
	tag    string
	region string
	client vpcClient
	log    *slog.Logger

	id        int
	subnetIDs map[string]int
}

func newManagedVPC(cfg vpcConfig, tag, region string, client vpcClient, log *slog.Logger) *managedVPC {
	return &managedVPC{
		cfg:       cfg,
		tag:       tag,
		region:    region,
		client:    client,
		log:       log,
		subnetIDs: map[string]int{},
	}
}

// ensure brings the VPC and its subnets into existence on demand.
// No-op when the cached ID is still valid; otherwise re-runs
// ensureAtConfigure to recreate the VPC. The reaper resets m.id to 0
// and clears m.subnetIDs when it deletes the VPC on last-Destroy, so
// a subsequent Provision needs this hook to self-heal instead of
// degrading workers to public-NIC-only (which silently breaks
// worker→cache reachability) — same FJB-10 shape as the PG and FW.
func (m *managedVPC) ensure(ctx context.Context) error {
	if m.id != 0 {
		return nil
	}
	m.log.Info("managed vpc: re-creating after teardown")
	return m.ensureAtConfigure(ctx)
}

// ensureAtConfigure finds-or-creates the VPC and all declared subnets.
// Eager at Configure (same rationale as firewall + PG): PAT-scope mistakes
// surface at startup instead of at the first job arrival.
func (m *managedVPC) ensureAtConfigure(ctx context.Context) error {
	existing, err := m.findVPC(ctx)
	if err != nil {
		return fmt.Errorf("find vpc: %w", err)
	}
	if existing != nil {
		return m.adoptExisting(ctx, existing)
	}
	return m.createFresh(ctx)
}

// adoptExisting records subnet IDs from an existing VPC and creates any
// configured subnets that don't yet exist on it. Treats labels as the
// match key — VPCs have no Tags field, so the label is the only ownership
// marker.
func (m *managedVPC) adoptExisting(ctx context.Context, v *linodego.VPC) error {
	m.id = v.ID
	for i := range v.Subnets {
		for name := range m.cfg.Subnets {
			if v.Subnets[i].Label == subnetLabel(m.tag, name) {
				m.subnetIDs[name] = v.Subnets[i].ID
			}
		}
	}
	for _, name := range sortedSubnetNames(m.cfg.Subnets) {
		if _, ok := m.subnetIDs[name]; ok {
			continue
		}
		sn, err := m.client.CreateVPCSubnet(ctx, linodego.VPCSubnetCreateOptions{
			Label: subnetLabel(m.tag, name),
			IPv4:  m.cfg.Subnets[name].IPv4,
		}, m.id)
		if err != nil {
			return fmt.Errorf("create vpc subnet %q: %w", name, err)
		}
		m.subnetIDs[name] = sn.ID
	}
	return nil
}

// createFresh creates the VPC plus all configured subnets in a single
// CreateVPC call (Linode supports inline subnet creation).
func (m *managedVPC) createFresh(ctx context.Context) error {
	names := sortedSubnetNames(m.cfg.Subnets)
	subnets := make([]linodego.VPCSubnetCreateOptions, 0, len(names))
	for _, name := range names {
		subnets = append(subnets, linodego.VPCSubnetCreateOptions{
			Label: subnetLabel(m.tag, name),
			IPv4:  m.cfg.Subnets[name].IPv4,
		})
	}
	created, err := m.client.CreateVPC(ctx, linodego.VPCCreateOptions{
		Label:   vpcLabel(m.cfg.Name, m.tag),
		Region:  m.region,
		Subnets: subnets,
	})
	if err != nil {
		return fmt.Errorf("create vpc: %w", err)
	}
	m.id = created.ID
	for i := range created.Subnets {
		for _, name := range names {
			if created.Subnets[i].Label == subnetLabel(m.tag, name) {
				m.subnetIDs[name] = created.Subnets[i].ID
				break
			}
		}
	}
	return nil
}

// findVPC matches by label + region. VPCs have no Tags field (like PGs);
// label is the ownership marker. Region scoping handles the unlikely case
// of two deployments with the same tag in different regions.
func (m *managedVPC) findVPC(ctx context.Context) (*linodego.VPC, error) {
	want := vpcLabel(m.cfg.Name, m.tag)
	vpcs, err := m.client.ListVPCs(ctx, nil)
	if err != nil {
		return nil, err
	}
	for i := range vpcs {
		if vpcs[i].Label == want && vpcs[i].Region == m.region {
			return &vpcs[i], nil
		}
	}
	return nil, nil
}

// workerSubnetID returns the Linode subnet ID workers should attach to.
// Zero when the VPC hasn't been provisioned yet — callers should treat
// that as "skip the VPC interface this round".
func (m *managedVPC) workerSubnetID() int {
	return m.subnetIDs[m.cfg.resolvedWorkerSubnet()]
}

// maybeCleanupVPC deletes the VPC iff no subnet has any attached linodes.
// Called from Linode.Destroy after DeleteInstance — the last instance
// going away naturally drives cleanup, same pattern as firewall + PG.
func (m *managedVPC) maybeCleanupVPC(ctx context.Context) {
	v, err := m.findVPC(ctx)
	if err != nil {
		m.log.Warn("managed vpc: lookup during cleanup", "err", err)
		return
	}
	if v == nil {
		return
	}
	full, err := m.client.GetVPC(ctx, v.ID)
	if err != nil {
		m.log.Warn("managed vpc: get during cleanup", "id", v.ID, "err", err)
		return
	}
	for i := range full.Subnets {
		if len(full.Subnets[i].Linodes) > 0 {
			return
		}
	}
	for i := range full.Subnets {
		if err := m.client.DeleteVPCSubnet(ctx, full.ID, full.Subnets[i].ID); err != nil {
			m.log.Warn("managed vpc: delete subnet during cleanup",
				"vpc", full.ID, "subnet", full.Subnets[i].ID, "err", err)
			return
		}
	}
	if err := m.client.DeleteVPC(ctx, full.ID); err != nil {
		m.log.Warn("managed vpc: delete during cleanup", "id", full.ID, "err", err)
		return
	}
	m.id = 0
	m.subnetIDs = map[string]int{}
	m.log.Info("managed vpc: deleted (no linodes remained on any subnet)", "id", full.ID)
}

// sortedSubnetNames returns the configured subnet names in alphabetical
// order. Deterministic — used by every iteration over Subnets so map
// iteration order can't introduce nondeterminism into CreateVPC payloads
// or the worker-subnet default selection.
func sortedSubnetNames(m map[string]subnetConfig) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Linode VPC and subnet labels: 1-64 chars from [A-Za-z0-9-] (no
// underscores or dots, unlike firewalls/PGs — the API rejects them).
// Separate sanitizer keeps the charset rules local to the VPC code.
const (
	vpcLabelMin = 1
	vpcLabelMax = 64
)

// vpcLabel renders the VPC label. When cfg.Name is set it's used verbatim
// (still sanitized for charset safety); otherwise the deployment tag drives
// the label with the fj-bellows prefix, matching firewall + PG conventions.
func vpcLabel(name, tag string) string {
	if name != "" {
		return sanitizeVPCLabel("", name, vpcLabelMin, vpcLabelMax)
	}
	return sanitizeVPCLabel("fj-bellows-", tag, vpcLabelMin, vpcLabelMax)
}

// subnetLabel namespaces the subnet within the VPC's label space so two
// deployments using the same subnet name (e.g. both "cache") on a shared
// VPC don't collide.
func subnetLabel(tag, name string) string {
	return sanitizeVPCLabel("fj-bellows-", tag+"-"+name, vpcLabelMin, vpcLabelMax)
}

// sanitizeVPCLabel applies the VPC charset (alnum + hyphen only) with the
// same long-tag truncation-by-SHA256-suffix scheme sanitizeLabel uses.
// Kept separate from sanitizeLabel because the charset is strictly tighter
// here — Linode rejects underscores and dots on VPC labels.
func sanitizeVPCLabel(prefix, tag string, minLen, maxLen int) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
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
	sum := sha256.Sum256([]byte(tag))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	head := candidate[:maxLen-len(suffix)]
	return head + suffix
}

// linodego.Client must satisfy our reduced interface; compile-time guard.
var _ vpcClient = (*linodego.Client)(nil)
