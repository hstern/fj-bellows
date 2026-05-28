package linode

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/linode/linodego"

	"github.com/hstern/fj-bellows/internal/transport/cachegateway"
)

// cacheClient is the slice of *linodego.Client the managed-cache code
// uses for the cache VM lifecycle. Bucket + Object Storage key
// operations live on bucketClient (composed separately).
type cacheClient interface {
	ListInstances(ctx context.Context, opts *linodego.ListOptions) ([]linodego.Instance, error)
	GetInstance(ctx context.Context, id int) (*linodego.Instance, error)
	CreateInstance(ctx context.Context, opts linodego.InstanceCreateOptions) (*linodego.Instance, error)
	DeleteInstance(ctx context.Context, id int) error
	ListInstanceConfigs(ctx context.Context, linodeID int, opts *linodego.ListOptions) ([]linodego.InstanceConfig, error)
}

// cacheConfig is the provider_config.cache sub-block.
//
// FJB-13: zot is a scratch registry workers address explicitly at
// `cache.fjb.internal:5000`, not a transparent pull-through cache.
// Pushes of intermediate build artifacts land in zot's S3 bucket;
// pulls of those artifacts come back from zot. Pushes that need to
// reach the canonical registry (Forgejo, Docker Hub, etc.) go direct
// to that registry from the worker, bypassing zot. There is no
// transparent redirect of any hostname — workers know about zot only
// via its hostname + TLS trust + /etc/hosts entry, and use it
// explicitly. The previous transparent-redirect machinery (sync
// extension, containerd hosts.toml mirror, FJB-7 reverse-tunnel,
// FJB-9 containerd-snapshotter) is gone.
type cacheConfig struct {
	// Type is the Linode instance type for the cache VM. Default is
	// g6-nanode-1 — sufficient for the typical small-team workload;
	// operators bump to g6-standard-1 (2 GB) under burst-pull pressure.
	Type string `yaml:"type"`

	// Image is the Linode image ID. Default is linode/debian13.
	Image string `yaml:"image"`

	// ZotVersion pins the zot binary release the cloud-init downloads.
	// Default is the version this PR was tested against; bump
	// deliberately to take a new zot.
	ZotVersion string `yaml:"zot_version"`

	// Upstream is a removed field, accepted only to surface a clear
	// deprecation error for operators copy-pasting old configs. The
	// transparent-redirect model it powered (zot's sync extension)
	// was retired in FJB-13. validate() rejects any non-nil value
	// with a migration note.
	Upstream *cacheUpstreamConfig `yaml:"upstream"`

	// TLS holds the fjb-managed CA persistence settings. The CA is
	// load-or-generate at Configure-time and signs the cache VM's
	// server cert. Persisting it across daemon restarts is what makes
	// adopt-existing safe: an adopted cache VM was signed by the same
	// CA that's still distributed to workers.
	TLS *cacheTLSConfig `yaml:"tls"`
}

// cacheUpstreamConfig is retained only so the YAML decoder doesn't
// trip on stale `cache.upstream:` blocks in old configs — validate()
// rejects with a clear deprecation message. Will be removed after
// the field stops appearing in operators' configs.
type cacheUpstreamConfig struct {
	URL string `yaml:"url"`
}

// cacheTLSConfig governs the fjb-managed CA persistence.
type cacheTLSConfig struct {
	// CADir is where ca-cert.pem + ca-key.pem live across daemon
	// restarts. Default is $XDG_CONFIG_HOME/fj-bellows/<tag>/cache-ca
	// (or the OS-specific equivalent via os.UserConfigDir). Operators
	// running fjb as a service should override to a stable location
	// like /var/lib/fj-bellows/<tag>/cache-ca so daemon restarts
	// don't churn the CA. Mode 0700 enforced at create.
	CADir string `yaml:"ca_dir"`
}

// Defaults applied when fields are left empty.
const (
	defaultCacheType       = "g6-nanode-1"
	defaultCacheImage      = "linode/debian13"
	defaultZotVersion      = "2.1.7"
	defaultCacheReadyFile  = "/var/lib/cloud/fj-bellows-cache.ready"
	defaultCacheSubnetName = "cache"
	defaultCacheSubnetCIDR = "10.0.0.0/24"
	defaultCacheHostname   = "cache.fjb.internal"
	defaultCachePort       = 5000
)

// validate is syntactic — required fields default if empty, no API
// calls. Real validation (bucket reachability, OS-enablement) happens
// at ensureAtConfigure when we hit the API.
func (c cacheConfig) validate() error {
	if c.Upstream != nil {
		return errors.New("cache.upstream is no longer supported (FJB-13): " +
			"zot is now a scratch registry workers address at " +
			"cache.fjb.internal:5000 directly, not a transparent pull-through " +
			"of an upstream registry — remove the cache.upstream block from " +
			"your config")
	}
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

// resolvedCADir returns where to persist the cache CA across daemon
// restarts. Operator override takes precedence; default is under
// os.UserConfigDir() (XDG-aware on Linux, ~/Library/Application
// Support on macOS) namespaced by deployment tag.
func (c cacheConfig) resolvedCADir(tag string) (string, error) {
	if c.TLS != nil && strings.TrimSpace(c.TLS.CADir) != "" {
		return c.TLS.CADir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve CA dir: %w (set cache.tls.ca_dir to override)", err)
	}
	return filepath.Join(base, "fj-bellows", sanitizePathSegment(tag), "cache-ca"), nil
}

// sanitizePathSegment strips characters that would be problematic in
// a filesystem path segment (slashes, NULs). Conservative — keep
// alnum + dash + dot + underscore.
func sanitizePathSegment(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, s)
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

	// workerVPCSubnet is the CIDR (e.g. "10.0.0.0/24") of the VPC subnet
	// workers attach to. Used at cache-create time to bake the
	// fjb-iptables FORWARD chain into cloud-init (FJB-98). Empty when
	// no VPC is configured — the iptables bake-in is then skipped.
	workerVPCSubnet string

	// orchestratorWGPubkey is the orchestrator's WG public key, set by
	// setOrchestratorWGPubkey before ensureAtConfigure runs (FJB-99
	// Phase A). The cache cloud-init bakes it in as [Peer].PublicKey so
	// wg-quick@wg0 brings up the tunnel on first boot referencing the
	// right orchestrator peer. Empty under ssh-mode deployments — the
	// WG bootstrap block is then omitted from cloud-init entirely.
	orchestratorWGPubkey string

	// linodeID is the cache VM's Linode ID. Populated by ensureAt-
	// Configure (find-or-adopt), cleared by maybeCleanupCache.
	linodeID int

	// adoptedExisting reports whether ensureAtConfigure adopted a pre-
	// existing cache VM (vs creating fresh). When true we skip bucket
	// + key creation — the existing VM is already running with its
	// baked-in creds; we just track it for cleanup. Daemon restart
	// thus leaves a working cache intact.
	adoptedExisting bool

	// caCertPEM is the trust anchor distributed to workers via the
	// multipart-MIME worker cloud-init wrap. Populated by ensureAt-
	// Configure (load-or-generate from cfg.TLS.CADir). Empty when the
	// cache stack isn't fully wired (e.g. early in setup or in tests
	// that skip the CA path).
	caCertPEM []byte

	// cacheVPCIP is the cache VM's IPv4 on the cache subnet. Looked
	// up lazily on first WorkerExtras call (so a fresh-create VM has
	// time to settle on its IP) and cached. Empty until then.
	cacheVPCIP string

	// transportMode mirrors the outer Linode.transportMode (FJB-65 /
	// FJB-74). Drives the worker cache-extras template selection.
	transportMode string

	// aclSnapshot is the source of AllowedIPs CIDRs the cache-gateway
	// worker cloud-init renders into `ip route replace ... via
	// <cacheVPCIP>` lines (FJB-88). The orchestrator owns the ACL
	// registry; this provider only reads a snapshot at provision time.
	// Modeled as an interface so the provider stays decoupled from the
	// concrete acl.Registry / acl.Snapshot types — orchestrator wiring
	// (FJB-90) injects an adapter. nil when no ACL source has been
	// wired (legacy ssh mode, or cache-gateway mode pre-FJB-90).
	aclSnapshot ACLSnapshotSource
}

// ACLSnapshotSource is the narrow read-only view of the ACL registry
// the worker cloud-init renderer needs: the de-duplicated, sorted set
// of CIDR strings that compose AllowedIPs at the moment Provision runs.
// FJB-90 supplies a real adapter over the orchestrator's acl.Registry;
// tests use stubs.
//
// Implementations must:
//   - return CIDRs in canonical string form ("192.168.0.0/24",
//     "10.0.0.5/32") so they paste verbatim into `ip route replace`,
//   - return them deduplicated and sorted (stable ordering keeps the
//     rendered cloud-init byte-stable for goldens and reduces churn
//     in cloud-init diff on tick),
//   - exclude the cache's own /32 (workers reach the cache via the
//     VPC, not via WG), per transport.md "Worker route derivation".
//
// Exported so cmd/fj-bellows can build an adapter without importing
// internal linode types. The provider holds it by interface only.
type ACLSnapshotSource interface {
	AllowedIPsCIDRs() []string
}

// setTransport propagates the top-level transport mode from the Linode
// provider into the managed cache so workerExtras can pick the right
// template at provision time. Called from the Linode provider after
// SetTransportMode has populated its own field.
func (m *managedCache) setTransport(mode string) {
	m.transportMode = mode
}

// setACLSource wires the ACL registry adapter the orchestrator built
// (FJB-90) into the managed cache. Idempotent — replacing nil with a
// real source is the common path; subsequent updates replace whatever
// was there. Called from Linode.SetACLSource so workerExtras can read
// the live AllowedIPs set on every Provision.
func (m *managedCache) setACLSource(src ACLSnapshotSource) {
	m.aclSnapshot = src
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
func (m *managedCache) setHardwareContext(firewallID, vpcSubnetID int, authorizedKey, workerVPCSubnet string) {
	m.firewallID = firewallID
	m.vpcSubnetID = vpcSubnetID
	m.authorizedKey = authorizedKey
	m.workerVPCSubnet = workerVPCSubnet
}

// setOrchestratorWGPubkey supplies the orchestrator's WG public key
// (FJB-99 Phase A). Called by the Linode provider after the daemon has
// loaded its own keypair from disk; the value lands in cloud-init at
// cache-create time so the cache's wg-quick references the right peer.
func (m *managedCache) setOrchestratorWGPubkey(pubkey string) {
	m.orchestratorWGPubkey = pubkey
}

// ensure brings the cache VM (and its bucket + scoped key)
// into existence on demand. No-op when the cached linodeID is still
// valid; otherwise re-runs ensureAtConfigure to recreate from
// scratch. The reaper resets linodeID to 0 when it deletes the VM
// on last-Destroy, so a subsequent Provision needs this hook to
// self-heal instead of erroring out with "cache linode not
// provisioned yet" in workerExtras — same FJB-10 shape as PG, FW,
// VPC. The CA dir is persistent across reaps, so the new VM is
// signed by the same CA workers already trust.
func (m *managedCache) ensure(ctx context.Context) error {
	if m.linodeID != 0 {
		return nil
	}
	m.log.Info("managed cache: re-creating after teardown")
	return m.ensureAtConfigure(ctx)
}

// ensureAtConfigure adopts an existing cache VM if one is tagged for
// this deployment, otherwise mints the bucket + scoped key, renders
// cloud-init, and creates the VM. Eager at Configure (same rationale
// as firewall + VPC: surface API + scope problems at startup). The CA
// is loaded or freshly generated from cfg.TLS.CADir before any branch
// — workers always need the CA PEM in WorkerExtras, even when we
// adopt an existing cache VM.
func (m *managedCache) ensureAtConfigure(ctx context.Context) error {
	caDir, err := m.cfg.resolvedCADir(m.tag)
	if err != nil {
		return err
	}
	pair, freshCA, err := loadOrGenerateCertPair(caDir, defaultCacheHostname)
	if err != nil {
		return fmt.Errorf("cache TLS: %w", err)
	}
	m.caCertPEM = pair.CACertPEM

	existing, err := m.findCacheLinode(ctx)
	if err != nil {
		return fmt.Errorf("find cache linode: %w", err)
	}
	if existing != nil {
		if freshCA {
			// CA dir was empty but a cache VM was found — the VM's
			// baked-in cert was signed by a CA we no longer have, and
			// workers would distribute a different CA. Reject loudly;
			// the operator picks: restore the CA dir from backup, or
			// run with -destroy-on-exit and let the next start
			// recreate the cache VM with the fresh CA.
			return fmt.Errorf("cache TLS: existing cache linode %d is signed by a CA that's not in %q. "+
				"Either restore the CA dir from backup or destroy the cache VM (Linode label %q) "+
				"and let the next start recreate it",
				existing.ID, caDir, existing.Label)
		}
		m.linodeID = existing.ID
		m.adoptedExisting = true
		m.log.Info("managed cache: adopted existing Linode", "id", existing.ID, "label", existing.Label, "ca_dir", caDir)
		return nil
	}

	return m.createFreshCacheLinode(ctx, pair)
}

// createFreshCacheLinode mints the bucket + key, renders cloud-init,
// and creates the cache VM. Extracted from ensureAtConfigure to keep
// the cyclomatic complexity of the parent under the linter's budget;
// the adopt branch returns early.
func (m *managedCache) createFreshCacheLinode(ctx context.Context, pair cacheCertPair) error {
	creds, err := m.bucket.ensureAtConfigure(ctx)
	if err != nil {
		return fmt.Errorf("bucket: %w", err)
	}
	iptablesScript, err := m.renderIPTablesForCacheGateway()
	if err != nil {
		return fmt.Errorf("render fjb-iptables: %w", err)
	}
	params := cacheCloudInitParams{
		Bucket:         creds.Bucket,
		Region:         creds.Region,
		Endpoint:       creds.Endpoint,
		AccessKey:      creds.AccessKey,
		SecretKey:      creds.SecretKey,
		ZotVersion:     m.cfg.resolvedZotVersion(),
		ReadyFile:      defaultCacheReadyFile,
		ServerCertPEM:  string(pair.ServerCertPEM),
		ServerKeyPEM:   string(pair.ServerKeyPEM),
		IPTablesScript: iptablesScript,
	}
	if m.transportMode == "cache-gateway" && m.orchestratorWGPubkey != "" {
		params.OrchestratorWGPubkey = m.orchestratorWGPubkey
		params.OrchestratorWGAddr = defaultOrchestratorWGAddr
		params.CacheWGAddr = defaultCacheWGAddr
		params.WGListenPort = defaultCacheWGListenPort
	}
	userData, err := renderCacheCloudInit(params)
	if err != nil {
		return fmt.Errorf("render cloud-init: %w", err)
	}
	rootPass, err := randomPassword(32)
	if err != nil {
		return fmt.Errorf("cache: generate root password: %w", err)
	}
	opts := m.buildCreateOpts(userData, rootPass)
	inst, err := m.client.CreateInstance(ctx, opts)
	if err != nil {
		return fmt.Errorf("create cache linode: %w", err)
	}
	m.linodeID = inst.ID
	m.log.Info("managed cache: created", "id", inst.ID, "label", inst.Label)
	return nil
}

// renderIPTablesForCacheGateway returns the fjb-iptables.sh content
// that the cache cloud-init drops into /usr/local/sbin/ and runs at
// first boot (FJB-98). Empty when the deployment isn't cache-gateway
// mode or the VPC subnet isn't known — the template then skips the
// iptables-persistent install + script execution entirely.
//
// At cache-create time the ACL registry hasn't been wired yet (that
// happens after Configure, when wgboot starts and calls SetACLSource),
// so AllowedIPs is empty here. The skeleton (sysctl + chain + the
// FJB-92 orchestrator → worker reverse-direction rule + DROP) still
// installs, which is what's load-bearing for the orchestrator-side
// dispatcher. Per-prefix worker-egress ACCEPT rules are added at
// runtime by the wgboot push path (separate ticket; see FJB-98
// "Out of scope").
func (m *managedCache) renderIPTablesForCacheGateway() (string, error) {
	if m.transportMode != "cache-gateway" || m.workerVPCSubnet == "" {
		return "", nil
	}
	orchAddr, err := netip.ParseAddr(defaultOrchestratorWGAddr)
	if err != nil {
		return "", fmt.Errorf("parse orchestrator WG addr %q: %w", defaultOrchestratorWGAddr, err)
	}
	return cachegateway.RenderCacheIPTables(cachegateway.Inputs{
		WorkerVPCSubnet:    m.workerVPCSubnet,
		OrchestratorWGAddr: orchAddr,
	})
}

// buildCreateOpts assembles the InstanceCreateOptions payload for the
// cache VM. Public stays primary so outbound (upstream sync, package
// mirrors, GitHub-zot download) takes the default route; the VPC NIC
// carries worker→cache pulls.
func (m *managedCache) buildCreateOpts(userData, rootPass string) linodego.InstanceCreateOptions {
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
		subID := m.vpcSubnetID
		opts.Interfaces = []linodego.InstanceConfigInterfaceCreateOptions{
			{Purpose: linodego.InterfacePurposePublic, Primary: true},
			{Purpose: linodego.InterfacePurposeVPC, SubnetID: &subID},
		}
	}
	return opts
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

// CacheStatus is the operator-facing snapshot of the managed cache state,
// returned by the control plane's GetCache RPC. Fields are populated from
// the in-memory managedCache plus an on-demand Linode API call for the live
// VM status (cheap; only fired on /cache requests, not in the hot path).
type CacheStatus struct {
	Present         bool
	AdoptedExisting bool
	LinodeID        int
	VPCIP           string
	BucketRegion    string
	BucketLabel     string
	VMState         string // from Linode API; empty if no VM or lookup failed
}

// Status returns the current cache snapshot. Safe to call before / after
// ensureAtConfigure (returns Present=false until linodeID is populated).
// The VPC IP is looked up on demand the first time the cache VM is known
// to exist — the lookup result is memoised on managedCache so subsequent
// callers (worker cloud-init, wgboot.Boot) read it synchronously.
func (m *managedCache) Status(ctx context.Context) CacheStatus {
	s := CacheStatus{
		Present:         m.linodeID != 0,
		AdoptedExisting: m.adoptedExisting,
		LinodeID:        m.linodeID,
		VPCIP:           m.cacheVPCIP,
	}
	if m.bucket != nil {
		s.BucketRegion = m.bucket.region
		s.BucketLabel = m.bucket.label
	}
	if s.Present {
		s.VMState = m.lookupLiveVMState(ctx)
		// Populate VPC IP eagerly the first time the cache is reachable.
		// wgboot.Boot wants this synchronously when constructing the
		// cache-gateway render inputs (FJB-90); workerExtras() reads the
		// same memoised field, so both call sites see a consistent value.
		if s.VPCIP == "" {
			s.VPCIP = m.ensureCacheVPCIP(ctx)
		}
	}
	return s
}

// lookupLiveVMState returns the Linode API's view of the cache VM's
// status string (or "" on failure — caller treats empty as transient).
func (m *managedCache) lookupLiveVMState(ctx context.Context) string {
	inst, err := m.client.GetInstance(ctx, m.linodeID)
	if err != nil {
		m.log.Debug("cache status: GetInstance failed", "id", m.linodeID, "err", err)
		return ""
	}
	if inst == nil {
		return ""
	}
	return string(inst.Status)
}

// ensureCacheVPCIP populates m.cacheVPCIP the first time the cache VM
// has a VPC NIC assigned. Returns the memoised value on success or "" on
// transient failure (VPC NIC still attaching; lookup retried next call).
func (m *managedCache) ensureCacheVPCIP(ctx context.Context) string {
	ip, err := m.lookupCacheVPCIP(ctx)
	if err != nil {
		m.log.Debug("cache status: VPC IP not yet assigned", "id", m.linodeID, "err", err)
		return ""
	}
	m.cacheVPCIP = ip
	return ip
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
// All fields except HostPrivateKey are required. Secret values
// (AccessKey/SecretKey/ServerKeyPEM/HostPrivateKey) reach the VM via
// the Linode Metadata service and never appear in process logs —
// render this only when the cache VM is about to be created.
type cacheCloudInitParams struct {
	Bucket         string
	Region         string
	Endpoint       string
	AccessKey      string
	SecretKey      string
	ZotVersion     string
	ReadyFile      string
	HostPrivateKey string

	// ServerCertPEM + ServerKeyPEM are the fjb-signed server cert and
	// key, written into /etc/zot/tls/ by the template. Workers trust
	// these via the CA distributed in the multipart worker cloud-init.
	ServerCertPEM string
	ServerKeyPEM  string

	// IPTablesScript is the rendered fjb-iptables.sh content (FJB-98)
	// embedded into the cloud-init under /usr/local/sbin/. Empty under
	// ssh-mode deployments — the template skips both the script
	// write_files entry and the iptables-persistent install when this
	// field is empty.
	IPTablesScript string

	// FJB-99 Phase A — WG bootstrap. When OrchestratorWGPubkey is set
	// the cache cloud-init installs wireguard + awscli, generates its
	// own keypair, writes /etc/wireguard/wg0.conf with the orchestrator
	// as the peer, brings up wg-quick@wg0, and publishes its pubkey to
	// s3://<Bucket>/wg-pubkey.txt for the orchestrator to read back
	// (Phase B). Empty under ssh-mode deployments — the WG block is
	// then skipped entirely.
	OrchestratorWGPubkey string
	OrchestratorWGAddr   string // e.g. "100.64.0.1"
	CacheWGAddr          string // e.g. "100.64.0.2"
	WGListenPort         int    // UDP, e.g. 51820
}

// renderCacheCloudInit fills the embedded template. Defaults to the
// constant ReadyFile when the caller leaves it empty, so the
// readiness-probe path is stable across configurations.
func renderCacheCloudInit(p cacheCloudInitParams) (string, error) {
	if err := validateCloudInitParams(p); err != nil {
		return "", err
	}
	if p.ReadyFile == "" {
		p.ReadyFile = defaultCacheReadyFile
	}
	tmpl, err := template.New("cache").Funcs(template.FuncMap{
		"b64enc": func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
		"indent": func(spaces int, s string) string {
			prefix := strings.Repeat(" ", spaces)
			lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
			for i, line := range lines {
				lines[i] = prefix + line
			}
			return strings.Join(lines, "\n")
		},
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

// validateCloudInitParams returns an error naming any missing required
// field. Extracted to keep renderCacheCloudInit under the linter's
// cyclomatic budget; the new TLS + Upstream params nudged it over.
func validateCloudInitParams(p cacheCloudInitParams) error {
	missing := []string{}
	if p.Bucket == "" {
		missing = append(missing, "Bucket")
	}
	if p.Region == "" {
		missing = append(missing, "Region")
	}
	if p.Endpoint == "" {
		missing = append(missing, "Endpoint")
	}
	if p.AccessKey == "" {
		missing = append(missing, "AccessKey")
	}
	if p.SecretKey == "" {
		missing = append(missing, "SecretKey")
	}
	if p.ZotVersion == "" {
		missing = append(missing, "ZotVersion")
	}
	if p.ServerCertPEM == "" {
		missing = append(missing, "ServerCertPEM")
	}
	if p.ServerKeyPEM == "" {
		missing = append(missing, "ServerKeyPEM")
	}
	if len(missing) > 0 {
		return fmt.Errorf("cache cloud-init: missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// workerExtrasData is what the worker cloud-init wrap needs: the
// trust anchor (CA cert PEM), the cache hostname workers should
// resolve, the cache VPC IP that hostname maps to, and the cache
// TLS port. FJB-13: no transparent-redirect mirror config is shipped
// any more — workers address zot at cache.fjb.internal explicitly.
type workerExtrasData struct {
	CACertPEM string
	CacheHost string
	CacheIP   string
	CachePort int

	// TransportMode mirrors config.Transport.Mode and selects which
	// extras template renders. Empty / "ssh" keeps the legacy
	// hosts-file + CA path; "cache-gateway" picks the FJB-88 template
	// (resolv.conf pointing at the orchestrator's WG IP + one
	// `ip route` per ACL-derived CIDR, no /etc/hosts cache entry).
	TransportMode string

	// AllowedIPsCIDRs is the deduplicated, sorted set of CIDR strings
	// the worker should route via the cache VM (FJB-88). One entry
	// becomes one `ip route replace <cidr> via <CacheIP>` line under
	// the cache-gateway template. Sourced from the ACL registry's
	// snapshot at Provision time; ignored by the legacy ssh template.
	// May be empty in tests; validateWorkerExtras enforces non-empty
	// under cache-gateway mode (workers need at least one route to
	// reach anything across WG).
	AllowedIPsCIDRs []string

	// OrchestratorWGAddr is the orchestrator's WG overlay address —
	// where the worker's resolv.conf points and where the DNS
	// responder (FJB-83) listens. Defaults to 100.64.0.1 (the
	// transport.md baseline overlay); a future configurable
	// transport.wg.overlay_prefix would feed this. Carried as a field
	// so a non-default deployment doesn't need a template change, even
	// though v1 hardcodes the default.
	OrchestratorWGAddr string
}

// defaultOrchestratorWGAddr is the default WG overlay address the
// orchestrator binds inside wireguard-go's netstack — the worker's
// single nameserver under cache-gateway mode. See transport.md:
// "Overlay addressing" — the /30 baseline is orchestrator=.1 / cache=.2
// inside RFC 6598 CGNAT space.
const defaultOrchestratorWGAddr = "100.64.0.1"

// defaultCacheWGAddr is the cache's address on the WG overlay (.2 in
// the orchestrator=.1/cache=.2 /30 baseline). FJB-99 Phase A bakes it
// into the cache cloud-init's wg0.conf [Interface].Address.
const defaultCacheWGAddr = "100.64.0.2"

// defaultCacheWGListenPort is the WireGuard project's de-facto default UDP
// port — mirrors config.DefaultWGListenPort but stays local to the
// provider so cache.go doesn't have to import internal/config.
const defaultCacheWGListenPort = 51820

// workerExtras returns the data the linode provider's Provision needs
// to wrap each worker's cloud-init with cache trust + hostname
// resolution. Looks up the cache VPC IP lazily (so a fresh-create
// cache VM has time to settle on its IP between Configure and the
// first Provision) and caches it on managedCache for subsequent
// calls. Returns an error when the IP isn't yet assigned — the
// orchestrator's reconcile loop retries Provision next tick, which is
// the right behavior since the IP is a precondition for worker→cache
// TLS.
func (m *managedCache) workerExtras(ctx context.Context) (workerExtrasData, error) {
	if len(m.caCertPEM) == 0 {
		return workerExtrasData{}, errors.New("workerExtras: cache CA not initialised")
	}
	if m.cacheVPCIP == "" {
		ip, err := m.lookupCacheVPCIP(ctx)
		if err != nil {
			return workerExtrasData{}, err
		}
		m.cacheVPCIP = ip
	}
	var cidrs []string
	if m.aclSnapshot != nil {
		// Defensive copy + sort/dedupe so the rendered cloud-init is
		// byte-stable regardless of what the snapshot source returns.
		// The ACLSnapshotSource contract already promises sorted +
		// deduped, but this is the boundary where breaking it would
		// silently produce noisy rerenders — cheap to enforce.
		raw := m.aclSnapshot.AllowedIPsCIDRs()
		cidrs = append(cidrs, raw...)
		slices.Sort(cidrs)
		cidrs = slices.Compact(cidrs)
	}
	return workerExtrasData{
		CACertPEM:          string(m.caCertPEM),
		CacheHost:          defaultCacheHostname,
		CacheIP:            m.cacheVPCIP,
		CachePort:          defaultCachePort,
		TransportMode:      m.transportMode,
		AllowedIPsCIDRs:    cidrs,
		OrchestratorWGAddr: defaultOrchestratorWGAddr,
	}, nil
}

// lookupCacheVPCIP queries the cache VM's configs for its VPC NIC IP.
// Returns an error when the VPC IP hasn't been assigned yet (e.g.
// VM still booting); the orchestrator's tick-driven Provision retry
// is the recovery path.
func (m *managedCache) lookupCacheVPCIP(ctx context.Context) (string, error) {
	if m.linodeID == 0 {
		return "", errors.New("lookupCacheVPCIP: cache linode not provisioned yet")
	}
	configs, err := m.client.ListInstanceConfigs(ctx, m.linodeID, nil)
	if err != nil {
		return "", fmt.Errorf("list configs for cache linode %d: %w", m.linodeID, err)
	}
	for i := range configs {
		for j := range configs[i].Interfaces {
			iface := configs[i].Interfaces[j]
			if iface.Purpose != linodego.InterfacePurposeVPC {
				continue
			}
			if iface.IPv4 == nil || iface.IPv4.VPC == "" {
				continue
			}
			return iface.IPv4.VPC, nil
		}
	}
	return "", fmt.Errorf("cache linode %d has no VPC interface IPv4 assigned yet", m.linodeID)
}

// preflightCacheRegion checks Object Storage is available for region
// before the linode provider creates any other deployment resources.
// Linode Object Storage isn't in every region (ca-tor today is one
// of the gaps); without this pre-flight the operator would discover
// the unavailability only after firewall + VPC creates succeeded
// and setupManagedCache errored with the same information. Fail
// early, fail clearly.
//
// Uses the same endpoint-then-clusters fallback the bucket lifecycle
// uses, since both surfaces are what advertise Object Storage's
// per-region presence.
func preflightCacheRegion(ctx context.Context, client bucketClient, region string) error {
	probe := newManagedBucket("preflight", region, "preflight", client, slog.Default())
	if _, err := probe.lookupEndpoint(ctx); err != nil {
		return fmt.Errorf("object storage not available for region %q: %w "+
			"(check https://www.linode.com/global-infrastructure/ for current availability "+
			"or pick a region with OS support)", region, err)
	}
	return nil
}

// linodego.Client must satisfy our reduced interface; compile-time guard.
var _ cacheClient = (*linodego.Client)(nil)
