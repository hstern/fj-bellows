package linode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/linode/linodego"
)

// bucketClient is the slice of *linodego.Client the managed-bucket code
// uses. Hand-rolled fake satisfies it in tests (per repo conventions —
// no codegen).
type bucketClient interface {
	GetObjectStorageBucket(ctx context.Context, regionOrCluster, label string) (*linodego.ObjectStorageBucket, error)
	CreateObjectStorageBucket(ctx context.Context, opts linodego.ObjectStorageBucketCreateOptions) (*linodego.ObjectStorageBucket, error)
	DeleteObjectStorageBucket(ctx context.Context, regionOrCluster, label string) error
	ListObjectStorageEndpoints(ctx context.Context, opts *linodego.ListOptions) ([]linodego.ObjectStorageEndpoint, error)
	ListObjectStorageClusters(ctx context.Context, opts *linodego.ListOptions) ([]linodego.ObjectStorageCluster, error)
	CreateObjectStorageKey(ctx context.Context, opts linodego.ObjectStorageKeyCreateOptions) (*linodego.ObjectStorageKey, error)
	DeleteObjectStorageKey(ctx context.Context, keyID int) error
	ListObjectStorageKeys(ctx context.Context, opts *linodego.ListOptions) ([]linodego.ObjectStorageKey, error)
}

// bucketCreds is what the cache cloud-init needs to talk to S3 — the S3
// endpoint URL, the bucket name, and a scoped access key pair limited to
// that bucket. The secret is exposed only once at create time so we keep
// it on the managedCache struct in memory until the cache VM is provisioned.
type bucketCreds struct {
	Bucket    string
	Region    string
	Endpoint  string // e.g. "https://us-ord-1.linodeobjects.com"
	AccessKey string
	SecretKey string
}

// managedBucket owns the Object Storage bucket + the scoped access key
// for one deployment. Lifecycle mirrors the other managed-* helpers:
// eager create at Configure, reap on last Destroy.
type managedBucket struct {
	tag    string
	region string
	label  string // bucket name
	client bucketClient
	log    *slog.Logger

	// keyID is the Linode-issued ID for the scoped Object Storage key.
	// Zero before ensureAtConfigure or after maybeCleanup.
	keyID int

	// endpoint is the S3 endpoint URL for the bucket's region, looked up
	// once at ensureAtConfigure.
	endpoint string

	// accessKey + secretKey are the scoped Object Storage credentials
	// minted at ensureAtConfigure. Stored so the orchestrator can sign
	// S3 GETs against the bucket for the FJB-99 wg-pubkey discovery
	// loop. Cleared on maybeCleanup.
	accessKey string
	secretKey string
}

func newManagedBucket(tag, region, label string, client bucketClient, log *slog.Logger) *managedBucket {
	return &managedBucket{
		tag:    tag,
		region: region,
		label:  label,
		client: client,
		log:    log,
	}
}

// ensureAtConfigure creates (or adopts) the bucket and mints a scoped
// access key. Returns the credentials the cache VM needs to talk to S3.
// Eager at Configure — surfaces PAT-scope mistakes (missing Object
// Storage: R/W) and Object-Storage-not-enabled-on-account at startup
// instead of at first job. The "feature not enabled" failure mode is
// Linode-side: CreateObjectStorageBucket returns a 403; we don't pre-
// probe (deferred to PR 2b for clearer-error UX).
func (m *managedBucket) ensureAtConfigure(ctx context.Context) (bucketCreds, error) {
	endpoint, err := m.lookupEndpoint(ctx)
	if err != nil {
		return bucketCreds{}, fmt.Errorf("lookup s3 endpoint for region %q: %w", m.region, err)
	}
	m.endpoint = endpoint

	existing, err := m.client.GetObjectStorageBucket(ctx, m.region, m.label)
	if err != nil && !isNotFound(err) {
		return bucketCreds{}, fmt.Errorf("get bucket %q: %w", m.label, err)
	}
	if existing == nil {
		if _, err := m.client.CreateObjectStorageBucket(ctx, linodego.ObjectStorageBucketCreateOptions{
			Region: m.region,
			Label:  m.label,
		}); err != nil {
			return bucketCreds{}, fmt.Errorf("create bucket %q in %q: %w", m.label, m.region, err)
		}
	}

	// The Object Storage key is per-deployment, scoped to this bucket
	// only. Linode reveals the secret_key exactly once (on create), so we
	// always create a fresh key here even when adopting an existing
	// bucket — daemon restart = new key, the orchestrator never persists
	// it. The old key is GC'd in maybeCleanup; left-over keys from prior
	// daemon lifetimes show up in ListObjectStorageKeys and are caught
	// by the cleanup-prefix sweep below.
	m.reapStaleKeys(ctx)

	keyLabel := keyLabelFor(m.tag)
	created, err := m.client.CreateObjectStorageKey(ctx, linodego.ObjectStorageKeyCreateOptions{
		Label: keyLabel,
		BucketAccess: &[]linodego.ObjectStorageKeyBucketAccess{{
			Region:      m.region,
			BucketName:  m.label,
			Permissions: "read_write",
		}},
	})
	if err != nil {
		return bucketCreds{}, fmt.Errorf("create scoped object storage key: %w", err)
	}
	m.keyID = created.ID
	m.accessKey = created.AccessKey
	m.secretKey = created.SecretKey

	return bucketCreds{
		Bucket:    m.label,
		Region:    m.region,
		Endpoint:  endpoint,
		AccessKey: created.AccessKey,
		SecretKey: created.SecretKey,
	}, nil
}

// lookupEndpoint finds the S3 endpoint URL for m.region. Tries the
// new /object-storage/endpoints API first (the path Linode's docs
// point at) but falls back to the legacy /object-storage/clusters
// API when endpoints returns null for the region — which is the case
// for every E3-type region today (most of them; only the legacy
// "us-east" cluster has a populated s3_endpoint there). Returns an
// error rather than guessing so misconfiguration is loud.
func (m *managedBucket) lookupEndpoint(ctx context.Context) (string, error) {
	if ep, err := m.lookupEndpointFromEndpointsAPI(ctx); err == nil && ep != "" {
		return ep, nil
	}
	if ep, err := m.lookupEndpointFromClustersAPI(ctx); err == nil && ep != "" {
		return ep, nil
	}
	return "", fmt.Errorf("no S3 endpoint advertised for region %q (checked /object-storage/endpoints and /object-storage/clusters)", m.region)
}

// lookupEndpointFromEndpointsAPI checks the new /object-storage/endpoints
// surface. Returns ("", nil) when no endpoint advertises an S3 URL for
// the region — that's a normal case (E3 regions return null here),
// distinct from a real API error.
func (m *managedBucket) lookupEndpointFromEndpointsAPI(ctx context.Context) (string, error) {
	eps, err := m.client.ListObjectStorageEndpoints(ctx, nil)
	if err != nil {
		return "", err
	}
	for i := range eps {
		if eps[i].Region != m.region {
			continue
		}
		if eps[i].S3Endpoint == nil || *eps[i].S3Endpoint == "" {
			continue
		}
		return normalizeEndpointURL(*eps[i].S3Endpoint), nil
	}
	return "", nil
}

// lookupEndpointFromClustersAPI falls back to the legacy clusters
// surface, which advertises a Domain for every region. Picks the
// first cluster matching m.region — when multiple clusters per region
// exist (e.g. us-iad-1 / us-iad-10), this prefers the lower-numbered
// cluster, which is fine for bucket creation since we pass `region`
// (not the cluster ID) in CreateObjectStorageBucketOptions.
func (m *managedBucket) lookupEndpointFromClustersAPI(ctx context.Context) (string, error) {
	cs, err := m.client.ListObjectStorageClusters(ctx, nil)
	if err != nil {
		return "", err
	}
	for i := range cs {
		if cs[i].Region != m.region {
			continue
		}
		if cs[i].Domain == "" {
			continue
		}
		return normalizeEndpointURL(cs[i].Domain), nil
	}
	return "", nil
}

// normalizeEndpointURL prepends https:// when the Linode API returns a
// bare hostname (the common case for legacy clusters).
func normalizeEndpointURL(s string) string {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return "https://" + s
}

// reapStaleKeys deletes any Object Storage keys whose label matches the
// deployment-scoped pattern. On a clean daemon start this is a no-op;
// on daemon restart it reaps the key minted by the previous lifetime so
// keys don't accumulate.
func (m *managedBucket) reapStaleKeys(ctx context.Context) {
	want := keyLabelFor(m.tag)
	keys, err := m.client.ListObjectStorageKeys(ctx, nil)
	if err != nil {
		m.log.Warn("managed bucket: list keys for reap", "err", err)
		return
	}
	for i := range keys {
		if keys[i].Label != want {
			continue
		}
		if err := m.client.DeleteObjectStorageKey(ctx, keys[i].ID); err != nil {
			m.log.Warn("managed bucket: delete stale key", "id", keys[i].ID, "err", err)
			continue
		}
		m.log.Info("managed bucket: reaped stale scoped key from prior daemon lifetime", "id", keys[i].ID)
	}
}

// maybeCleanupBucket reaps the scoped key and the bucket. The key goes
// first (cheap and unconditionally safe). The bucket only goes if it's
// empty — Linode rejects DeleteObjectStorageBucket on a non-empty bucket
// with a 400. Callers that want destructive purge behavior need to walk
// + delete objects first (out of scope for v1; PR 2b adds the
// retain_after_destroy + force-empty knobs).
func (m *managedBucket) maybeCleanupBucket(ctx context.Context) {
	if m.keyID != 0 {
		if err := m.client.DeleteObjectStorageKey(ctx, m.keyID); err != nil {
			m.log.Warn("managed bucket: delete scoped key during cleanup", "id", m.keyID, "err", err)
		} else {
			m.log.Info("managed bucket: deleted scoped key", "id", m.keyID)
		}
		m.keyID = 0
	}
	if err := m.client.DeleteObjectStorageBucket(ctx, m.region, m.label); err != nil {
		// Common, non-fatal: bucket has cached layers, won't delete.
		// Log at INFO not WARN — the operator typically wants the cache
		// data to survive across deployments anyway.
		m.log.Info("managed bucket: delete skipped (non-empty or already gone)", "bucket", m.label, "err", err)
		return
	}
	m.log.Info("managed bucket: deleted", "bucket", m.label)
}

// keyLabelFor returns the Object Storage key label for a deployment.
// Linode key labels accept a wider charset than VPC labels; reuse the
// firewall/PG sanitizer with the same fj-bellows- prefix so deployments
// with weird tags still get a clean unique label.
func keyLabelFor(tag string) string {
	const keyLabelMin = 1
	const keyLabelMax = 50 // conservative under Linode's 100-char ceiling
	return sanitizeLabel("fj-bellows-cache-", tag, keyLabelMin, keyLabelMax)
}

// bucketLabelFor returns the Object Storage bucket name for a deployment.
// Bucket names follow S3 DNS rules: lowercase alnum + hyphen, 3-63 chars,
// must start/end with alnum. We always prefix `fjb-cache-` to namespace
// and lowercase + sanitize the deployment tag.
func bucketLabelFor(tag string) string {
	const bucketLabelMin = 3
	const bucketLabelMax = 63
	candidate := "fjb-cache-" + s3Sanitize(tag)
	if len(candidate) > bucketLabelMax {
		// Long tag — reuse the firewall/PG SHA-256 suffix scheme so
		// distinct long tags don't collide, then S3-sanitize the
		// result (sanitizeLabel allows underscores/dots).
		candidate = s3Sanitize(sanitizeLabel("fjb-cache-", tag, bucketLabelMin, bucketLabelMax))
	}
	for len(candidate) < bucketLabelMin {
		candidate += "-x"
	}
	candidate = strings.Trim(candidate, "-")
	if candidate == "" {
		candidate = "fjb-cache"
	}
	return candidate
}

// s3Sanitize lowercases the input and replaces anything outside
// [a-z0-9-] with '-'. S3 bucket names are case-sensitive but Linode
// normalizes to lowercase and refuses uppercase, so we collapse here.
func s3Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, s)
}

// isNotFound reports whether err is a Linode 404, used in find-or-create
// flows where "not found" is normal and shouldn't bubble as a real error.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var le *linodego.Error
	if asLinodeError(err, &le) {
		return le.Code == 404
	}
	// linodego sometimes wraps 404s as strings; treat the substring
	// match as a fallback so we don't false-fail on bucket adoption.
	return strings.Contains(err.Error(), "[404]") || strings.Contains(err.Error(), "Not found")
}

// asLinodeError unwraps err into a *linodego.Error if possible. Returns
// true iff the unwrap succeeds. Separate from isNotFound so other call
// sites can match on Code without re-implementing the unwrap. Uses
// errors.As so wrapped Linode errors still match.
func asLinodeError(err error, out **linodego.Error) bool {
	if err == nil {
		return false
	}
	return errors.As(err, out)
}

// linodego.Client must satisfy our reduced interface; compile-time guard.
var _ bucketClient = (*linodego.Client)(nil)
