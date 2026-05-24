package linode

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"text/template"

	"github.com/linode/linodego"
)

// cacheClient is the slice of *linodego.Client the managed-cache code
// uses for the cache VM lifecycle. Bucket + Object Storage key
// operations live on bucketClient (composed separately).
type cacheClient interface {
	ListInstances(ctx context.Context, opts *linodego.ListOptions) ([]linodego.Instance, error)
	CreateInstance(ctx context.Context, opts linodego.InstanceCreateOptions) (*linodego.Instance, error)
	DeleteInstance(ctx context.Context, id int) error
}

// cacheConfig is the provider_config.cache sub-block. Minimal surface
// for PR 2a — `image`, `type`, and `zot_version` are the knobs operators
// realistically tune. Sub-blocks for upstream sync, retention policy,
// volume, and bucket retention land in PR 2b.
type cacheConfig struct {
	// Type is the Linode instance type for the cache VM. Default is
	// g6-nanode-1 — sufficient for the typical small-team workload;
	// operators bump to g6-standard-1 (2 GB) under burst-pull pressure.
	Type string `yaml:"type"`

	// Image is the Linode image ID. Default is linode/debian12.
	Image string `yaml:"image"`

	// ZotVersion pins the zot binary release the cloud-init downloads.
	// Default is the version this PR was tested against; bump
	// deliberately to take a new zot.
	ZotVersion string `yaml:"zot_version"`
}

// Defaults applied when fields are left empty.
const (
	defaultCacheType       = "g6-nanode-1"
	defaultCacheImage      = "linode/debian12"
	defaultZotVersion      = "2.1.7"
	defaultCacheReadyFile  = "/var/lib/cloud/fj-bellows-cache.ready"
	defaultCacheSubnetName = "cache"
	defaultCacheSubnetCIDR = "10.0.0.0/24"
)

// validate is syntactic — required fields default if empty, no API
// calls. Real validation (bucket reachability, OS-enablement) happens
// at ensureAtConfigure when we hit the API.
func (c cacheConfig) validate() error {
	// All fields are optional today; the validator exists for symmetry
	// with the other managed-* configs and to keep room for future
	// validation (e.g. zot_version SemVer parse).
	return nil
}

// resolvedType / Image / ZotVersion substitute defaults for empty
// fields. Kept separate from validate() so the original yaml value
// stays observable for tests / debugging.
func (c cacheConfig) resolvedType() string {
	if c.Type != "" {
		return c.Type
	}
	return defaultCacheType
}

func (c cacheConfig) resolvedImage() string {
	if c.Image != "" {
		return c.Image
	}
	return defaultCacheImage
}

func (c cacheConfig) resolvedZotVersion() string {
	if c.ZotVersion != "" {
		return c.ZotVersion
	}
	return defaultZotVersion
}

// managedCache coordinates the bucket + cache-VM lifecycle for one
// deployment. The cache VM is a Linode instance separate from workers,
// tagged with `<tag>-cache` (NOT the deployment tag) so the
// orchestrator's List(tag) — exact-match on the worker tag — doesn't
// see it. The deployment-tag-prefix cleanup sweep in e2e still catches
// it because `<tag>-cache` starts with `<tag>`.
type managedCache struct {
	cfg    cacheConfig
	tag    string
	region string
	client cacheClient
	bucket *managedBucket
	log    *slog.Logger

	// firewallID + vpcSubnetID are populated by setupManagedCache from
	// the already-Configured fw + vpc helpers; zero values mean "no
	// firewall / no VPC attach" and the cache VM gets the Linode
	// default (public NIC, no firewall) — fine for tests against fakes.
	firewallID    int
	vpcSubnetID   int
	authorizedKey string

	// linodeID is the cache VM's Linode ID. Populated by ensureAt-
	// Configure (find-or-adopt), cleared by maybeCleanupCache.
	linodeID int

	// adoptedExisting reports whether ensureAtConfigure adopted a pre-
	// existing cache VM (vs creating fresh). When true we skip bucket
	// + key creation — the existing VM is already running with its
	// baked-in creds; we just track it for cleanup. Daemon restart
	// thus leaves a working cache intact.
	adoptedExisting bool
}

func newManagedCache(cfg cacheConfig, tag, region string, client cacheClient, bucket *managedBucket, log *slog.Logger) *managedCache {
	return &managedCache{
		cfg:    cfg,
		tag:    tag,
		region: region,
		client: client,
		bucket: bucket,
		log:    log,
	}
}

// setHardwareContext supplies the firewall/VPC/SSH key the cache VM
// should be wired into. Called by the Linode provider's setupManaged-
// Cache after the firewall + VPC helpers have run. Kept separate from
// the constructor because the provider creates the managedCache before
// the firewall + VPC helpers exist on l, and one-shot setters keep the
// dependency direction one-way.
func (m *managedCache) setHardwareContext(firewallID, vpcSubnetID int, authorizedKey string) {
	m.firewallID = firewallID
	m.vpcSubnetID = vpcSubnetID
	m.authorizedKey = authorizedKey
}

// ensureAtConfigure adopts an existing cache VM if one is tagged for
// this deployment, otherwise mints the bucket + scoped key, renders
// cloud-init, and creates the VM. Eager at Configure (same rationale
// as firewall + VPC: surface API + scope problems at startup).
func (m *managedCache) ensureAtConfigure(ctx context.Context) error {
	existing, err := m.findCacheLinode(ctx)
	if err != nil {
		return fmt.Errorf("find cache linode: %w", err)
	}
	if existing != nil {
		m.linodeID = existing.ID
		m.adoptedExisting = true
		m.log.Info("managed cache: adopted existing Linode", "id", existing.ID, "label", existing.Label)
		return nil
	}

	creds, err := m.bucket.ensureAtConfigure(ctx)
	if err != nil {
		return fmt.Errorf("bucket: %w", err)
	}

	userData, err := renderCacheCloudInit(cacheCloudInitParams{
		Bucket:     creds.Bucket,
		Region:     creds.Region,
		Endpoint:   creds.Endpoint,
		AccessKey:  creds.AccessKey,
		SecretKey:  creds.SecretKey,
		ZotVersion: m.cfg.resolvedZotVersion(),
		ReadyFile:  defaultCacheReadyFile,
	})
	if err != nil {
		return fmt.Errorf("render cloud-init: %w", err)
	}

	rootPass, err := randomPassword(32)
	if err != nil {
		return fmt.Errorf("cache: generate root password: %w", err)
	}
	booted := true
	opts := linodego.InstanceCreateOptions{
		Region:   m.region,
		Type:     m.cfg.resolvedType(),
		Image:    m.cfg.resolvedImage(),
		Label:    cacheLinodeLabel(m.tag),
		Tags:     []string{cacheLinodeTag(m.tag)},
		Booted:   &booted,
		RootPass: rootPass,
		Metadata: &linodego.InstanceMetadataOptions{
			UserData: base64.StdEncoding.EncodeToString([]byte(userData)),
		},
	}
	if m.authorizedKey != "" {
		opts.AuthorizedKeys = []string{m.authorizedKey}
	}
	if m.firewallID != 0 {
		opts.FirewallID = m.firewallID
	}
	if m.vpcSubnetID != 0 {
		// Explicit two-NIC: public + VPC. Public stays primary so
		// outbound (upstream sync, package mirrors, GitHub-zot
		// download) takes the default route; the VPC NIC carries
		// worker→cache pulls in PR 2b.
		subID := m.vpcSubnetID
		opts.Interfaces = []linodego.InstanceConfigInterfaceCreateOptions{
			{Purpose: linodego.InterfacePurposePublic, Primary: true},
			{Purpose: linodego.InterfacePurposeVPC, SubnetID: &subID},
		}
	}

	inst, err := m.client.CreateInstance(ctx, opts)
	if err != nil {
		return fmt.Errorf("create cache linode: %w", err)
	}
	m.linodeID = inst.ID
	m.log.Info("managed cache: created", "id", inst.ID, "label", inst.Label)
	return nil
}

// findCacheLinode looks up the deployment's cache VM by tag. Cache VMs
// carry `<tag>-cache` and NOT the worker tag, so this is a distinct
// lookup from the orchestrator's List(tag).
func (m *managedCache) findCacheLinode(ctx context.Context) (*linodego.Instance, error) {
	want := cacheLinodeTag(m.tag)
	insts, err := m.client.ListInstances(ctx, nil)
	if err != nil {
		return nil, err
	}
	for i := range insts {
		if slices.Contains(insts[i].Tags, want) {
			return &insts[i], nil
		}
	}
	return nil, nil
}

// maybeCleanupCache reaps the cache VM + the scoped bucket key. Called
// from Linode.Destroy on the last worker teardown (same per-instance
// hook that reaps firewall + VPC). The bucket itself is left intact —
// cached layers are valuable across deployments; PR 2b adds the
// retain_after_destroy knob for explicit destruction.
func (m *managedCache) maybeCleanupCache(ctx context.Context) {
	if m.linodeID != 0 {
		if err := m.client.DeleteInstance(ctx, m.linodeID); err != nil {
			m.log.Warn("managed cache: delete linode during cleanup", "id", m.linodeID, "err", err)
		} else {
			m.log.Info("managed cache: deleted linode", "id", m.linodeID)
		}
		m.linodeID = 0
	}
	if !m.adoptedExisting {
		// We minted the key in this lifetime — reap it. Bucket
		// deletion is best-effort (will fail with 400 if non-empty,
		// logged at INFO).
		m.bucket.maybeCleanupBucket(ctx)
	}
}

// cacheLinodeLabel is the Linode instance label for the cache VM. The
// instance-label charset is wider than VPC labels (underscores + dots
// allowed) so reuse the firewall/PG sanitizer; max length 64 per Linode.
func cacheLinodeLabel(tag string) string {
	const labelMin = 1
	const labelMax = 64
	return sanitizeLabel("fj-bellows-cache-", tag, labelMin, labelMax)
}

// cacheLinodeTag is the deployment-cache tag stamped on the cache VM.
// It is intentionally DIFFERENT from the deployment tag — the
// orchestrator's List(tag) is exact-match, so a cache VM tagged
// `<tag>-cache` (not `<tag>`) is invisible to the worker pool while
// still caught by the e2e's prefix-based destroy_tagged sweep.
func cacheLinodeTag(tag string) string {
	return tag + "-cache"
}

//go:embed cache-cloud-init.yaml.tmpl
var cacheCloudInitTemplate string

// cacheCloudInitParams are the inputs to the cache cloud-init template.
// All fields are required except HostPrivateKey (optional pre-pinned
// ed25519 host key, mirror of the worker pattern). Secret values
// (AccessKey/SecretKey/HostPrivateKey) reach the VM via the Linode
// Metadata service and never appear in process logs — render this only
// when the cache VM is about to be created.
type cacheCloudInitParams struct {
	Bucket         string
	Region         string
	Endpoint       string
	AccessKey      string
	SecretKey      string
	ZotVersion     string
	ReadyFile      string
	HostPrivateKey string
}

// renderCacheCloudInit fills the embedded template. Defaults to the
// constant ReadyFile when the caller leaves it empty, so the
// readiness-probe path is stable across configurations.
func renderCacheCloudInit(p cacheCloudInitParams) (string, error) {
	if p.Bucket == "" || p.Region == "" || p.Endpoint == "" ||
		p.AccessKey == "" || p.SecretKey == "" || p.ZotVersion == "" {
		return "", errors.New("cache cloud-init: missing required field (Bucket/Region/Endpoint/AccessKey/SecretKey/ZotVersion)")
	}
	if p.ReadyFile == "" {
		p.ReadyFile = defaultCacheReadyFile
	}
	tmpl, err := template.New("cache").Funcs(template.FuncMap{
		"b64enc": func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
	}).Parse(cacheCloudInitTemplate)
	if err != nil {
		return "", fmt.Errorf("parse cache cloud-init template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		return "", fmt.Errorf("execute cache cloud-init template: %w", err)
	}
	return buf.String(), nil
}

// linodego.Client must satisfy our reduced interface; compile-time guard.
var _ cacheClient = (*linodego.Client)(nil)
