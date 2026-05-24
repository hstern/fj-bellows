// Package linode implements the provider.Provider interface for Linode.
//
// Linode bills whole hours rounded up, so it reports BillingHourlyRoundUp and
// the core keeps VMs warm for the paid hour. Provisioning passes cloud-init via
// the Linode Metadata service (user-data) and tags every instance so reconcile
// and the orphan sweep can find them.
package linode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/linode/linodego"
	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

// config is the provider_config subtree for Linode.
type config struct {
	Region           string                `yaml:"region"`
	Type             string                `yaml:"type"`
	Image            string                `yaml:"image"`
	Token            string                `yaml:"token"`
	FirewallID       int                   `yaml:"firewall_id"`
	Firewall         *firewallConfig       `yaml:"firewall"`
	PlacementGroupID int                   `yaml:"placement_group_id"`
	PlacementGroup   *placementGroupConfig `yaml:"placement_group"`
	VPC              *vpcConfig            `yaml:"vpc"`
	Cache            *cacheConfig          `yaml:"cache"`
}

// Linode is the provider implementation.
type Linode struct {
	cfg    config
	client linodego.Client
	tag    string // cfg.Tag from the orchestrator, captured in Configure

	// fw is non-nil when managed-firewall mode is enabled. Created at
	// Configure time; nil when in firewall_id mode (or no firewall).
	fw *managedFirewall

	// pg is non-nil when managed-placement-group mode is enabled. Same
	// lifecycle shape: Configure-time create, last-Destroy reaps. See #51.
	pg *managedPlacementGroup

	// vpc is non-nil when the managed `vpc:` block is set. Workers gain a
	// VPC interface in addition to their public NIC; eager Configure-time
	// create, reap on last Destroy — same shape as fw and pg.
	vpc *managedVPC

	// cache is non-nil when the managed `cache:` block is set. Cache lives
	// outside the worker pool: a separate Linode tagged `<tag>-cache` (so
	// List(tag) never sees it), an Object Storage bucket, and a scoped
	// access key wired into the cache VM's cloud-init. Setting `cache:`
	// without `vpc:` auto-synthesizes the VPC (FJB-6 design).
	cache *managedCache
}

func init() {
	provider.Register("linode", func() provider.Provider { return &Linode{} })
}

// Configure decodes the opaque node and prepares the API client.
//
// For the managed-firewall mode (`firewall:` block), Configure also:
//   - resolves the allow_inbound sentinels (`auto`, `github-actions`) and
//     fails fast on any error (silent fallback to literal CIDRs would risk
//     locking out the orchestrator from its own workers);
//   - creates the Cloud Firewall via the Linode API and starts the refresh
//     goroutine.
//
// All firewall work happens here, at startup. Failures surface immediately
// (PAT-scope misconfiguration, network blips, missing sentinels) instead
// of deferred to the first Provision call when the first job lands.
// See #26.
func (l *Linode) Configure(ctx context.Context, tag string, node yaml.Node) error {
	if err := node.Decode(&l.cfg); err != nil {
		return fmt.Errorf("linode: decode provider_config: %w", err)
	}
	if err := l.cfg.validateAll(); err != nil {
		return err
	}
	// Auto-synthesize VPC when cache: is set and vpc: is absent. Done
	// AFTER validateAll so an operator-supplied vpc: block is checked
	// for typos before we silently substitute it.
	l.cfg.autoSynthesizeVPCForCache()
	client := linodego.NewClient(nil)
	client.SetToken(l.cfg.Token)
	l.client = client
	l.tag = tag

	if l.cfg.Firewall != nil {
		if err := l.setupManagedFirewall(ctx, tag); err != nil {
			return err
		}
	}
	if l.cfg.PlacementGroup != nil {
		if err := l.setupManagedPlacementGroup(ctx, tag); err != nil {
			return err
		}
	}
	if l.cfg.VPC != nil {
		if err := l.setupManagedVPC(ctx, tag); err != nil {
			return err
		}
	}
	// Cache last — it depends on VPC (for subnet ID) and firewall (for
	// the deployment-shared firewall attach). autoSynthesizeVPCForCache
	// above guarantees l.vpc is non-nil here when l.cfg.Cache is set.
	if l.cfg.Cache != nil {
		if err := l.setupManagedCache(ctx, tag); err != nil {
			return err
		}
	}
	return nil
}

// validateAll runs the syntactic checks on the decoded provider_config:
// required fields populated, mutex constraints between the managed and
// attach-by-id modes for both firewall and placement-group, and the
// per-block validators. Pure validation — no API calls.
func (c config) validateAll() error {
	if err := c.validateRequiredFields(); err != nil {
		return err
	}
	if err := c.validateMutexes(); err != nil {
		return err
	}
	return c.validateSubBlocks()
}

// validateMutexes rejects configs that set both the managed-block and
// the attach-by-id field for any resource type.
func (c config) validateMutexes() error {
	if c.Firewall != nil && c.FirewallID != 0 {
		return errors.New("linode: provider_config: `firewall` and `firewall_id` are mutually exclusive")
	}
	if c.PlacementGroup != nil && c.PlacementGroupID != 0 {
		return errors.New("linode: provider_config: `placement_group` and `placement_group_id` are mutually exclusive")
	}
	return nil
}

// validateSubBlocks delegates to each non-nil sub-block's validator
// and wraps the error with the YAML field name for operator clarity.
func (c config) validateSubBlocks() error {
	if c.Firewall != nil {
		if err := c.Firewall.validate(); err != nil {
			return fmt.Errorf("linode: firewall: %w", err)
		}
	}
	if c.PlacementGroup != nil {
		if err := c.PlacementGroup.validate(); err != nil {
			return fmt.Errorf("linode: placement_group: %w", err)
		}
	}
	if c.VPC != nil {
		if err := c.VPC.validate(); err != nil {
			return fmt.Errorf("linode: vpc: %w", err)
		}
	}
	if c.Cache != nil {
		if err := c.Cache.validate(); err != nil {
			return fmt.Errorf("linode: cache: %w", err)
		}
	}
	return nil
}

// autoSynthesizeVPCForCache populates cfg.VPC with a sensible default
// when cache: is set and vpc: was left empty. The cache VM binds to a
// VPC NIC and workers will reach it over that NIC (PR 2b), so a VPC is
// load-bearing for the cache — synthesizing a default keeps `cache: {}`
// a one-line opt-in instead of forcing the operator to type out both
// blocks just to get the documented default behavior.
func (c *config) autoSynthesizeVPCForCache() {
	if c.Cache == nil || c.VPC != nil {
		return
	}
	c.VPC = &vpcConfig{
		Subnets: map[string]subnetConfig{
			defaultCacheSubnetName: {IPv4: defaultCacheSubnetCIDR},
		},
	}
}

// validateRequiredFields collects all missing required Linode provider_config
// fields and reports them in one go (operator can fix them in one pass).
func (c config) validateRequiredFields() error {
	var missing []string
	if c.Region == "" {
		missing = append(missing, "region")
	}
	if c.Type == "" {
		missing = append(missing, "type")
	}
	if c.Image == "" {
		missing = append(missing, "image")
	}
	if c.Token == "" {
		missing = append(missing, "token")
	}
	if len(missing) > 0 {
		return fmt.Errorf("linode: provider_config missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// setupManagedFirewall constructs the managedFirewall, resolves the sentinels,
// creates/updates the firewall, and starts the refresh goroutine. Extracted
// from Configure so each piece has a clear name and the parent function stays
// under the cyclomatic-complexity budget.
func (l *Linode) setupManagedFirewall(ctx context.Context, tag string) error {
	fw := newManagedFirewall(*l.cfg.Firewall, tag, &l.client, slog.Default())
	if err := fw.primeResolved(ctx); err != nil {
		return fmt.Errorf("linode: firewall: %w", err)
	}
	if err := fw.ensureAtConfigure(ctx); err != nil {
		return fmt.Errorf("linode: firewall: %w", err)
	}
	fw.startRefreshLoop()
	l.fw = fw
	return nil
}

// setupManagedPlacementGroup constructs the managedPlacementGroup and
// creates the Linode placement group at Configure time. Same eager-create
// rationale as the firewall: PAT-scope mistakes surface at startup.
func (l *Linode) setupManagedPlacementGroup(ctx context.Context, tag string) error {
	pg := newManagedPlacementGroup(*l.cfg.PlacementGroup, tag, l.cfg.Region, &l.client, slog.Default())
	if err := pg.ensureAtConfigure(ctx); err != nil {
		return fmt.Errorf("linode: placement_group: %w", err)
	}
	l.pg = pg
	return nil
}

// setupManagedVPC constructs the managedVPC and creates the Linode VPC +
// declared subnets at Configure time. Same eager-create rationale as the
// firewall / PG: PAT-scope mistakes (e.g. PAT missing VPCs: R/W) surface
// at startup, not at the first job arrival.
func (l *Linode) setupManagedVPC(ctx context.Context, tag string) error {
	v := newManagedVPC(*l.cfg.VPC, tag, l.cfg.Region, &l.client, slog.Default())
	if err := v.ensureAtConfigure(ctx); err != nil {
		return fmt.Errorf("linode: vpc: %w", err)
	}
	l.vpc = v
	return nil
}

// setupManagedCache constructs the managedBucket + managedCache and
// stands up the cache VM + Object Storage bucket + scoped access key at
// Configure time. Order matters: this runs AFTER setupManagedFirewall +
// setupManagedVPC so the cache VM can attach to both. PAT-scope
// mistakes (Object Storage: R/W, account not enabled for Object
// Storage) surface here at startup.
func (l *Linode) setupManagedCache(ctx context.Context, tag string) error {
	bucket := newManagedBucket(tag, l.cfg.Region, bucketLabelFor(tag), &l.client, slog.Default())
	cache := newManagedCache(*l.cfg.Cache, tag, l.cfg.Region, &l.client, bucket, slog.Default())
	var fwID int
	switch {
	case l.fw != nil:
		fwID = l.fw.id
	case l.cfg.FirewallID != 0:
		fwID = l.cfg.FirewallID
	}
	// VPC is guaranteed non-nil here by autoSynthesizeVPCForCache; a
	// missing worker subnet would be a synthesis bug, so a zero subnet
	// ID degrades gracefully (cache VM gets a public NIC only) rather
	// than crashing.
	var subID int
	if l.vpc != nil {
		subID = l.vpc.workerSubnetID()
	}
	// authorized_keys is left empty in PR 2a — the cache VM has no SSH
	// key wired in by fjb; operators use Linode Lish for break-glass.
	// PR 2b will pass the orchestrator's public key here.
	cache.setHardwareContext(fwID, subID, "")
	if err := cache.ensureAtConfigure(ctx); err != nil {
		return fmt.Errorf("linode: cache: %w", err)
	}
	l.cache = cache
	return nil
}

// Provision creates a tagged Linode with the rendered cloud-init as user-data.
func (l *Linode) Provision(ctx context.Context, spec provider.Spec) (provider.Instance, error) {
	rootPass, err := randomPassword(32)
	if err != nil {
		return provider.Instance{}, err
	}
	// If the managed cache is enabled, wrap the orchestrator-rendered
	// cloud-init in a multipart MIME message that adds a second
	// cloud-config part with the cache CA trust, /etc/hosts entry, and
	// containerd pull-only mirror config. cloud-init merges the two
	// parts natively. Provider-side wrap keeps the orchestrator and
	// the `bootstrap` package free of provider-specific concerns.
	userData := spec.UserData
	if l.cache != nil {
		extras, xerr := l.cache.workerExtras(ctx)
		if xerr != nil {
			return provider.Instance{}, fmt.Errorf("linode: cache worker extras: %w", xerr)
		}
		wrapped, werr := wrapWorkerUserDataForCache(spec.UserData, extras)
		if werr != nil {
			return provider.Instance{}, fmt.Errorf("linode: wrap worker user-data: %w", werr)
		}
		userData = wrapped
	}
	booted := true
	opts := linodego.InstanceCreateOptions{
		Region:   l.cfg.Region,
		Type:     l.cfg.Type,
		Image:    l.cfg.Image,
		Label:    spec.Name,
		Tags:     []string{spec.Tag},
		RootPass: rootPass,
		Booted:   &booted,
		Metadata: &linodego.InstanceMetadataOptions{
			UserData: base64.StdEncoding.EncodeToString([]byte(userData)),
		},
	}
	if key := strings.TrimSpace(spec.AuthorizedKey); key != "" {
		opts.AuthorizedKeys = []string{key}
	}
	switch {
	case l.fw != nil:
		opts.FirewallID = l.fw.id
	case l.cfg.FirewallID != 0:
		opts.FirewallID = l.cfg.FirewallID
	}
	switch {
	case l.pg != nil:
		opts.PlacementGroup = &linodego.InstanceCreatePlacementGroupOptions{ID: l.pg.id}
	case l.cfg.PlacementGroupID != 0:
		opts.PlacementGroup = &linodego.InstanceCreatePlacementGroupOptions{ID: l.cfg.PlacementGroupID}
	}
	// VPC: workers keep their public NIC (they need public egress for
	// registries, package mirrors, etc.) and gain a VPC NIC on the
	// resolved worker subnet. Explicit Interfaces requires us to declare
	// the public NIC too — Linode doesn't auto-add it once we set the
	// field. Skip the VPC NIC if the subnet ID isn't populated; ensureAt-
	// Configure should have set it, so this is defensive.
	if l.vpc != nil {
		if subID := l.vpc.workerSubnetID(); subID != 0 {
			opts.Interfaces = []linodego.InstanceConfigInterfaceCreateOptions{
				{Purpose: linodego.InterfacePurposePublic, Primary: true},
				{Purpose: linodego.InterfacePurposeVPC, SubnetID: &subID},
			}
		}
	}
	inst, err := l.client.CreateInstance(ctx, opts)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("linode: create instance: %w", err)
	}
	return toInstance(*inst), nil
}

// Destroy deletes the instance with the given ID.
//
// When managed-firewall / managed-placement-group modes are on, the last
// Destroy in a deployment triggers cleanup of those resources (each is
// removed once no devices/members remain attached). -destroy-on-exit
// naturally flows through here per instance, so we get cleanup for free
// without a Provider.Shutdown hook.
func (l *Linode) Destroy(ctx context.Context, id string) error {
	n, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("linode: bad instance id %q: %w", id, err)
	}
	if err := l.client.DeleteInstance(ctx, n); err != nil {
		return fmt.Errorf("linode: delete instance %d: %w", n, err)
	}
	// Cache first: it owns its own Linode + bucket + scoped key, all of
	// which can be torn down without any dependency on the worker
	// firewall / VPC reap below. Doing cache first also avoids a window
	// where the cache VM is still attached to a VPC subnet we're about
	// to delete.
	if l.cache != nil {
		l.cache.maybeCleanupCache(ctx)
	}
	if l.fw != nil {
		l.fw.maybeCleanupFirewall(ctx)
	}
	if l.pg != nil {
		l.pg.maybeCleanupPlacementGroup(ctx)
	}
	if l.vpc != nil {
		l.vpc.maybeCleanupVPC(ctx)
	}
	return nil
}

// List returns all instances carrying tag.
func (l *Linode) List(ctx context.Context, tag string) ([]provider.Instance, error) {
	insts, err := l.client.ListInstances(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("linode: list instances: %w", err)
	}
	var out []provider.Instance
	for _, in := range insts {
		if slices.Contains(in.Tags, tag) {
			out = append(out, toInstance(in))
		}
	}
	return out, nil
}

// BillingModel reports hourly rounding.
func (l *Linode) BillingModel() provider.BillingModel {
	return provider.BillingHourlyRoundUp
}

func toInstance(in linodego.Instance) provider.Instance {
	var ip string
	if len(in.IPv4) > 0 && in.IPv4[0] != nil {
		ip = in.IPv4[0].String()
	}
	var created time.Time
	if in.Created != nil {
		created = *in.Created
	}
	var tag string
	if len(in.Tags) > 0 {
		tag = in.Tags[0]
	}
	return provider.Instance{
		ID:        strconv.Itoa(in.ID),
		Name:      in.Label,
		IPv4:      ip,
		CreatedAt: created,
		Tag:       tag,
	}
}

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#%^&*"

// randomPassword returns a strong random root password. It is never used to log
// in (the orchestrator authenticates with an SSH key) but Linode requires one.
func randomPassword(n int) (string, error) {
	b := make([]byte, n)
	limit := big.NewInt(int64(len(passwordAlphabet)))
	for i := range b {
		idx, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("linode: generate password: %w", err)
		}
		b[i] = passwordAlphabet[idx.Int64()]
	}
	return string(b), nil
}
