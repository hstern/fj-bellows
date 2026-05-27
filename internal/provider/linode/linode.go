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
	"sync"
	"sync/atomic"
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

	// sshAuthorizedKey is populated by SetSSHAuthorizedKey before
	// Configure runs. The cache VM bakes this into authorized_keys so
	// the operator can ssh in for debugging — matches what workers
	// already get via spec.AuthorizedKey at Provision time. The cache
	// VM never dials back to the orchestrator (no tunnel); this is
	// strictly inbound-debug access.
	sshAuthorizedKey string

	// transportMode is the active transport architecture (config.Transport.Mode),
	// captured before Configure via SetTransportMode. Drives the firewall
	// rule synthesis: empty / "ssh" keeps the legacy tcp/22 ACCEPT;
	// "cache-gateway" (FJB-54) synthesizes IPsec ACCEPT (udp/500, udp/4500,
	// ESP) instead.
	transportMode string

	// workersInFlight counts Provision calls that have entered the
	// CreateInstance path but not yet returned (success or failure).
	// Surfaced via Info() for operator debugging — pairs with the
	// orchestrator's own pending counter from the other side.
	workersInFlight atomic.Int64

	// capacityFull tracks recent "capacity full" 400s from
	// CreateInstance over the last 24h as a small ring of timestamps.
	// Surfaced via Info() so the operator can correlate a stuck pool
	// with capacity pressure without scraping logs. Bounded; we only
	// keep timestamps, not the full error bodies.
	capacityFull capacityFullRing
}

// capacityFullRing is a tiny bounded rolling counter of "capacity full"
// timestamps over the last 24h. The ring caps memory at
// capacityFullRingMax entries — well above any plausible incident — and
// the count() method ages out entries older than the window each call.
type capacityFullRing struct {
	mu sync.Mutex
	// at is the timestamps of recent capacity-full events, oldest first.
	at []time.Time
}

// capacityFullRingMax bounds the ring at a sane upper limit so a runaway
// capacity-full storm can't grow unbounded. Far above any plausible
// 24h incident; the typical reading is in the single digits.
const capacityFullRingMax = 1024

// capacityFullWindow is the rolling window count() reports over.
const capacityFullWindow = 24 * time.Hour

// note records that one capacity-full 400 fired right now, dropping
// anything older than the rolling window and dropping the oldest entry
// when the ring is at the bound.
func (r *capacityFullRing) note(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune(now)
	if len(r.at) >= capacityFullRingMax {
		// Drop oldest.
		r.at = r.at[1:]
	}
	r.at = append(r.at, now)
}

// count returns the number of capacity-full events within the rolling
// window ending at now. Prunes aged-out entries as a side effect.
func (r *capacityFullRing) count(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune(now)
	return len(r.at)
}

// prune drops entries older than now-capacityFullWindow. Caller holds mu.
// Entries can arrive out of chronological order in principle (Provision
// is called concurrently and the wall clock is sampled per-call), so we
// scan-and-filter rather than truncating a prefix.
func (r *capacityFullRing) prune(now time.Time) {
	cutoff := now.Add(-capacityFullWindow)
	kept := r.at[:0]
	for _, t := range r.at {
		if !t.Before(cutoff) {
			kept = append(kept, t)
		}
	}
	r.at = kept
}

// SetSSHAuthorizedKey supplies the orchestrator's SSH public key
// (authorized_keys line) so the managed cache VM accepts the
// operator's ssh. Call from cmd/fj-bellows before Configure. No-op
// for non-Linode providers; harmless when cache is disabled.
func (l *Linode) SetSSHAuthorizedKey(authKey string) {
	l.sshAuthorizedKey = authKey
}

// SetTransportMode propagates the top-level transport mode
// (config.Transport.Mode) into the Linode provider before Configure
// runs. Drives firewall rule synthesis: legacy "ssh"/"" keeps tcp/22
// ACCEPT; "cache-gateway" (FJB-54) switches to IPsec ACCEPT instead.
// Duck-typed call from cmd/fj-bellows — providers that don't implement
// this method are unaffected (the docker provider has no firewall).
func (l *Linode) SetTransportMode(mode string) {
	l.transportMode = mode
}

// CacheStatus returns the managed-cache snapshot consumed by the control
// plane's GetCache RPC. Returns nil when no `cache:` block is configured;
// the control handler then reports Present=false to the wire. The Linode
// API is queried on demand for live VM status — cheap, not on a hot path.
func (l *Linode) CacheStatus(ctx context.Context) *CacheStatus {
	if l.cache == nil {
		return nil
	}
	s := l.cache.Status(ctx)
	return &s
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

	// Pre-flight Object Storage region availability. Runs BEFORE the
	// setupManaged* sequence so a region that doesn't support OS
	// (e.g. ca-tor today) fails Configure with a clear error and
	// zero resources created — without this, firewall + VPC would
	// already exist by the time setupManagedCache hits the OS API
	// and errored with the same information. Must run AFTER l.client
	// is initialised above.
	if l.cfg.Cache != nil {
		if err := preflightCacheRegion(ctx, &l.client, l.cfg.Region); err != nil {
			return fmt.Errorf("linode: cache: %w", err)
		}
	}

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
	fw := newManagedFirewall(*l.cfg.Firewall, tag, &l.client, slog.Default(), l.transportMode)
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
	// FJB-13: zot is a scratch registry; the orchestrator no longer
	// dials the cache VM, so the SSH key in authorized_keys is for
	// operator debugging only (the dispatcher's tunnel is gone). The
	// key is supplied via SetSSHAuthorizedKey from cmd/fj-bellows;
	// empty when no SSH was configured (docker provider).
	cache.setHardwareContext(fwID, subID, l.sshAuthorizedKey)
	if err := cache.ensureAtConfigure(ctx); err != nil {
		return fmt.Errorf("linode: cache: %w", err)
	}
	l.cache = cache
	return nil
}

// Provision creates a tagged Linode with the rendered cloud-init as user-data.
//
// Before constructing the create payload, every managed resource the
// orchestrator owns gets a lazy ensure(): firewall, placement group,
// VPC, cache. Each is a no-op when its cached ID is still valid;
// after a last-Destroy cascade (when the reaper has cleared the IDs)
// the ensure() calls re-create the missing resources so the next
// CreateInstance sees valid attach-refs instead of zero. Without
// this, the orchestrator wedges in a 10s retry loop sending
// PlacementGroup.ID=0 to Linode and getting a 400 every time —
// FJB-10. The order matches Configure: fw → pg → vpc → cache (cache
// reads vpc.workerSubnetID).
func (l *Linode) Provision(ctx context.Context, spec provider.Spec) (provider.Instance, error) {
	l.workersInFlight.Add(1)
	defer l.workersInFlight.Add(-1)
	if err := l.ensureManagedResources(ctx); err != nil {
		return provider.Instance{}, err
	}
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
	l.applyManagedResourceAttachments(&opts)
	inst, err := l.client.CreateInstance(ctx, opts)
	if err != nil && isPlacementGroupFull(err) {
		// FJB-31: surface "capacity full" pressure through Info() for
		// the operator. We record regardless of enforcement mode — a
		// strict-PG operator equally wants to know capacity pressure
		// is mounting, even if we won't auto-retry.
		l.capacityFull.note(time.Now())
	}
	if err != nil && l.shouldRetryWithoutPlacementGroup(err) {
		// FJB-11: with enforcement=flexible the operator's intent is
		// "best-effort PG, don't block on it". Linode's API treats
		// PG-full as a hard 400 anyway, so we honor flexible at the
		// orchestrator level by dropping the PG attach for this one
		// Linode and retrying. The PG fills back up at its own pace;
		// this single worker just doesn't get the anti-affinity slot.
		slog.Default().Warn(
			"placement group at capacity; provisioning this worker without PG attach (enforcement: flexible)",
			"tag", spec.Tag, "label", spec.Name,
		)
		opts.PlacementGroup = nil
		inst, err = l.client.CreateInstance(ctx, opts)
	}
	if err != nil {
		return provider.Instance{}, fmt.Errorf("linode: create instance: %w", err)
	}
	return toInstance(*inst), nil
}

// shouldRetryWithoutPlacementGroup applies the FJB-11 flexible-
// enforcement semantic: a PG-full error on a managed PG with
// enforcement=flexible warrants one retry without the PG attach.
// Strict enforcement bubbles the error (the operator explicitly
// chose to prefer no-worker over no-anti-affinity). Operator-managed
// PGs (placement_group_id) are left alone — fjb doesn't know the
// operator's intent for that group.
func (l *Linode) shouldRetryWithoutPlacementGroup(err error) bool {
	if l.pg == nil {
		return false
	}
	if l.pg.cfg.resolvedPolicy() != linodego.PlacementGroupPolicyFlexible {
		return false
	}
	return isPlacementGroupFull(err)
}

// isPlacementGroupFull pattern-matches the Linode 400 surfaced when
// the placement group has hit its per-region max_members ceiling.
// We match on the human-readable substring rather than a structured
// field because the Linode API doesn't expose a typed error code for
// this case as of writing. linodego.Error wraps it; the bare-string
// fallback covers cases where the error has already been wrapped
// into a generic error chain (errors.As also unwraps via
// asLinodeError, so this is belt-and-suspenders).
func isPlacementGroupFull(err error) bool {
	if err == nil {
		return false
	}
	const marker = "Placement Group is at its full capacity"
	var le *linodego.Error
	if asLinodeError(err, &le) {
		return le.Code == 400 && strings.Contains(le.Message, marker)
	}
	return strings.Contains(err.Error(), marker)
}

// applyManagedResourceAttachments stamps the firewall + placement
// group + VPC attach-refs onto the create options. Extracted from
// Provision to keep its cyclomatic complexity under the linter
// budget once ensureManagedResources joined the chain. The VPC
// branch keeps its defensive workerSubnetID()!=0 guard: even with
// the FJB-10 ensure() in place, a Configure-time synthesis bug
// could still leave it zero, and a worker without a VPC NIC is
// better than a Provision that errors.
func (l *Linode) applyManagedResourceAttachments(opts *linodego.InstanceCreateOptions) {
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
	if l.vpc != nil {
		if subID := l.vpc.workerSubnetID(); subID != 0 {
			opts.Interfaces = []linodego.InstanceConfigInterfaceCreateOptions{
				{Purpose: linodego.InterfacePurposePublic, Primary: true},
				{Purpose: linodego.InterfacePurposeVPC, SubnetID: &subID},
			}
		}
	}
}

// ensureManagedResources is the FJB-10 self-heal hook: on each
// Provision call, walk the managed-resource chain and let each
// resource re-create itself if its reaper has cleared the ID. No-op
// in the common case (steady-state, ids non-zero); load-bearing
// after a last-Destroy cascade has fired and a fresh job has just
// arrived. Order matches Configure.
func (l *Linode) ensureManagedResources(ctx context.Context) error {
	if l.fw != nil {
		if err := l.fw.ensure(ctx); err != nil {
			return fmt.Errorf("linode: re-create firewall: %w", err)
		}
	}
	if l.pg != nil {
		if err := l.pg.ensure(ctx); err != nil {
			return fmt.Errorf("linode: re-create placement group: %w", err)
		}
	}
	if l.vpc != nil {
		if err := l.vpc.ensure(ctx); err != nil {
			return fmt.Errorf("linode: re-create vpc: %w", err)
		}
	}
	if l.cache != nil {
		if err := l.cache.ensure(ctx); err != nil {
			return fmt.Errorf("linode: re-create cache: %w", err)
		}
	}
	return nil
}

// Destroy deletes the instance with the given ID.
//
// When managed-firewall / managed-placement-group modes are on, the last
// Destroy in a deployment triggers cleanup of those resources (each is
// removed once no devices/members remain attached). -destroy-on-exit
// naturally flows through here per instance, so we get cleanup for free
// without a Provider.Shutdown hook.
//
// FJB-12: the managed cache VM is intentionally NOT reaped here.
// Worker count goes to zero on idle teardown but the operator's
// intent — declared by setting `cache:` in config — is for the cache
// to outlive the worker fleet. Reaping it on every idle-to-empty
// transition (a) burned 3-5 min of cache boot on the next job after
// any idle period and (b) created the FJB-12 deadlock window where
// Provision needed the cache VPC IP from a VM that had just been
// reaped. With the cache kept warm, neither happens. Bucket + scoped
// key stay alive alongside it (they're owned by `maybeCleanupCache`
// and only get reaped when it does). The cache VM is still
// re-createable on demand via cache.ensure() (FJB-10), so it self-
// heals if it vanishes for any other reason (Linode incident, manual
// delete). Operators wanting an explicit teardown delete it via the
// Linode console; a future fjb flag could automate that if it
// becomes a common operation. Firewall and VPC reapers still fire
// correctly even though the cache VM remains: both gate on "no
// devices/linodes attached", and the cache VM keeps them in use, so
// they stay too.
func (l *Linode) Destroy(ctx context.Context, id string) error {
	n, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("linode: bad instance id %q: %w", id, err)
	}
	if err := l.client.DeleteInstance(ctx, n); err != nil {
		return fmt.Errorf("linode: delete instance %d: %w", n, err)
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
