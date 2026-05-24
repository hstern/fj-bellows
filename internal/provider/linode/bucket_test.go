package linode

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/linode/linodego"
)

const (
	testBucketRegion   = "us-ord"
	testBucketEndpoint = "https://us-ord-1.linodeobjects.com"
)

// fakeBucketClient is a hand-rolled bucketClient (per repo conventions
// — no codegen). Stores buckets by (region, label) and Object Storage
// keys by ID. Tests can pre-seed `buckets` and `keys` to exercise the
// adopt-existing paths and seed `endpoints` to pick the right region.
type fakeBucketClient struct {
	mu        sync.Mutex
	buckets   map[string]*linodego.ObjectStorageBucket // key: region|label
	keys      map[int]*linodego.ObjectStorageKey
	endpoints []linodego.ObjectStorageEndpoint
	clusters  []linodego.ObjectStorageCluster
	nextKeyID int

	// errors injected for specific paths to test error handling.
	getBucketErr     error
	createBucketErr  error
	deleteBucketErr  error
	listEndpointsErr error
	listClustersErr  error
	createKeyErr     error

	getBucketCalls     int
	createBucketCalls  int
	deleteBucketCalls  int
	listEndpointsCalls int
	listClustersCalls  int
	createKeyCalls     int
	deleteKeyCalls     int
	listKeysCalls      int
}

func newFakeBucketClient() *fakeBucketClient {
	ep := testBucketEndpoint
	return &fakeBucketClient{
		buckets: map[string]*linodego.ObjectStorageBucket{},
		keys:    map[int]*linodego.ObjectStorageKey{},
		endpoints: []linodego.ObjectStorageEndpoint{
			{Region: testBucketRegion, S3Endpoint: &ep},
		},
		clusters: []linodego.ObjectStorageCluster{
			// Matches the real Linode API shape: one cluster per
			// region, advertised via Domain even when the new
			// /endpoints surface returns null for that region.
			{ID: testBucketRegion + "-1", Region: testBucketRegion, Domain: testBucketRegion + "-1.linodeobjects.com"},
		},
	}
}

func bucketKey(region, label string) string { return region + "|" + label }

func (f *fakeBucketClient) GetObjectStorageBucket(_ context.Context, region, label string) (*linodego.ObjectStorageBucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getBucketCalls++
	if f.getBucketErr != nil {
		return nil, f.getBucketErr
	}
	b, ok := f.buckets[bucketKey(region, label)]
	if !ok {
		// Mirror linodego's 404 representation enough that isNotFound
		// recognizes it: a non-nil *linodego.Error with Code=404.
		return nil, &linodego.Error{Code: 404, Message: "not found"}
	}
	cp := *b
	return &cp, nil
}

func (f *fakeBucketClient) CreateObjectStorageBucket(_ context.Context, opts linodego.ObjectStorageBucketCreateOptions) (*linodego.ObjectStorageBucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createBucketCalls++
	if f.createBucketErr != nil {
		return nil, f.createBucketErr
	}
	b := &linodego.ObjectStorageBucket{
		Label:  opts.Label,
		Region: opts.Region,
	}
	f.buckets[bucketKey(opts.Region, opts.Label)] = b
	return b, nil
}

func (f *fakeBucketClient) DeleteObjectStorageBucket(_ context.Context, region, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteBucketCalls++
	if f.deleteBucketErr != nil {
		return f.deleteBucketErr
	}
	delete(f.buckets, bucketKey(region, label))
	return nil
}

func (f *fakeBucketClient) ListObjectStorageEndpoints(_ context.Context, _ *linodego.ListOptions) ([]linodego.ObjectStorageEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listEndpointsCalls++
	if f.listEndpointsErr != nil {
		return nil, f.listEndpointsErr
	}
	return append([]linodego.ObjectStorageEndpoint(nil), f.endpoints...), nil
}

func (f *fakeBucketClient) ListObjectStorageClusters(_ context.Context, _ *linodego.ListOptions) ([]linodego.ObjectStorageCluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listClustersCalls++
	if f.listClustersErr != nil {
		return nil, f.listClustersErr
	}
	return append([]linodego.ObjectStorageCluster(nil), f.clusters...), nil
}

func (f *fakeBucketClient) CreateObjectStorageKey(_ context.Context, opts linodego.ObjectStorageKeyCreateOptions) (*linodego.ObjectStorageKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createKeyCalls++
	if f.createKeyErr != nil {
		return nil, f.createKeyErr
	}
	f.nextKeyID++
	k := &linodego.ObjectStorageKey{
		ID:        f.nextKeyID,
		Label:     opts.Label,
		AccessKey: "AK-" + opts.Label,
		SecretKey: "SK-" + opts.Label,
	}
	f.keys[k.ID] = k
	return k, nil
}

func (f *fakeBucketClient) DeleteObjectStorageKey(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteKeyCalls++
	delete(f.keys, id)
	return nil
}

func (f *fakeBucketClient) ListObjectStorageKeys(_ context.Context, _ *linodego.ListOptions) ([]linodego.ObjectStorageKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listKeysCalls++
	out := make([]linodego.ObjectStorageKey, 0, len(f.keys))
	for _, k := range f.keys {
		out = append(out, *k)
	}
	return out, nil
}

func TestBucketEnsureAtConfigureCreatesFresh(t *testing.T) {
	ctx := context.Background()
	fake := newFakeBucketClient()
	m := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fake, slog.Default())

	creds, err := m.ensureAtConfigure(ctx)
	if err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	if creds.Bucket != "fjb-cache-test-tag" || creds.Region != testBucketRegion {
		t.Errorf("creds = %+v", creds)
	}
	if creds.Endpoint != testBucketEndpoint {
		t.Errorf("endpoint = %q, want %q", creds.Endpoint, testBucketEndpoint)
	}
	if creds.AccessKey == "" || creds.SecretKey == "" {
		t.Errorf("missing key material in creds: %+v", creds)
	}
	if fake.createBucketCalls != 1 {
		t.Errorf("CreateBucket calls = %d, want 1", fake.createBucketCalls)
	}
	if fake.createKeyCalls != 1 {
		t.Errorf("CreateKey calls = %d, want 1", fake.createKeyCalls)
	}
	if m.keyID == 0 {
		t.Errorf("keyID not recorded for cleanup")
	}
}

func TestBucketEnsureAtConfigureAdoptsExistingBucket(t *testing.T) {
	ctx := context.Background()
	fake := newFakeBucketClient()
	label := "fjb-cache-test-tag"
	fake.buckets[bucketKey(testBucketRegion, label)] = &linodego.ObjectStorageBucket{
		Label: label, Region: testBucketRegion,
	}
	m := newManagedBucket("test-tag", testBucketRegion, label, fake, slog.Default())

	if _, err := m.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	if fake.createBucketCalls != 0 {
		t.Errorf("CreateBucket should not run when bucket exists (got %d calls)", fake.createBucketCalls)
	}
	// We still mint a fresh scoped key — Linode reveals secret_key once
	// at create time, so a daemon restart needs a fresh one.
	if fake.createKeyCalls != 1 {
		t.Errorf("CreateKey calls = %d, want 1", fake.createKeyCalls)
	}
}

func TestBucketEnsureAtConfigureFailsOnUnknownRegion(t *testing.T) {
	ctx := context.Background()
	fake := newFakeBucketClient()
	fake.endpoints = nil // no endpoint advertised on /endpoints
	fake.clusters = nil  // and none on the /clusters fallback either
	m := newManagedBucket("test-tag", "fr-par", "fjb-cache-test-tag", fake, slog.Default())

	_, err := m.ensureAtConfigure(ctx)
	if err == nil {
		t.Fatal("expected error when region has no advertised endpoint")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("error should mention endpoint, got: %v", err)
	}
}

func TestBucketEndpointFallsBackToClustersWhenEndpointsAPIReturnsNull(t *testing.T) {
	// Matches the real Linode behavior: /object-storage/endpoints
	// returns the region but with s3_endpoint=null for E3-type
	// regions (most of them). Lookup must transparently fall back to
	// /object-storage/clusters and pick the Domain from there.
	ctx := context.Background()
	fake := newFakeBucketClient()
	// Endpoints API: region listed but no s3_endpoint.
	fake.endpoints = []linodego.ObjectStorageEndpoint{
		{Region: testBucketRegion, S3Endpoint: nil},
	}
	// Clusters API has the real domain — pre-seeded by newFakeBucketClient.

	m := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fake, slog.Default())
	creds, err := m.ensureAtConfigure(ctx)
	if err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	want := "https://" + testBucketRegion + "-1.linodeobjects.com"
	if creds.Endpoint != want {
		t.Errorf("endpoint = %q, want %q (clusters-fallback)", creds.Endpoint, want)
	}
	if fake.listClustersCalls != 1 {
		t.Errorf("clusters fallback should fire exactly once, got %d", fake.listClustersCalls)
	}
}

func TestBucketEndpointPrefersEndpointsAPIWhenS3EndpointPresent(t *testing.T) {
	// When /endpoints returns a non-null s3_endpoint we use it
	// directly and don't fall back. Verifies the precedence order.
	ctx := context.Background()
	fake := newFakeBucketClient() // default: endpoints API has us-ord S3 URL
	m := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fake, slog.Default())

	creds, err := m.ensureAtConfigure(ctx)
	if err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	if creds.Endpoint != testBucketEndpoint {
		t.Errorf("endpoint = %q, want %q (endpoints API)", creds.Endpoint, testBucketEndpoint)
	}
	if fake.listClustersCalls != 0 {
		t.Errorf("clusters fallback should NOT fire when endpoints has S3 URL, got %d", fake.listClustersCalls)
	}
}

func TestBucketReapStaleKeys(t *testing.T) {
	ctx := context.Background()
	fake := newFakeBucketClient()
	// Seed two prior-lifetime keys for this deployment plus one for
	// another deployment we should leave alone. nextKeyID is bumped
	// past the seeded IDs so CreateObjectStorageKey (called below by
	// ensureAtConfigure) doesn't reuse a numeric ID — that would
	// fool an ID-based "still present?" check.
	fake.nextKeyID = 200
	fake.keys[101] = &linodego.ObjectStorageKey{ID: 101, Label: keyLabelFor("test-tag")}
	fake.keys[102] = &linodego.ObjectStorageKey{ID: 102, Label: keyLabelFor("test-tag")}
	fake.keys[103] = &linodego.ObjectStorageKey{ID: 103, Label: keyLabelFor("other-tag")}

	m := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fake, slog.Default())
	if _, err := m.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}

	// Both 101 and 102 should be reaped; 103 (other deployment) untouched.
	if _, ok := fake.keys[101]; ok {
		t.Errorf("stale key 101 was not reaped")
	}
	if _, ok := fake.keys[102]; ok {
		t.Errorf("stale key 102 was not reaped")
	}
	if _, ok := fake.keys[103]; !ok {
		t.Errorf("other-deployment key 103 was incorrectly reaped")
	}
	// And exactly one freshly-minted test-tag key remains (the one
	// CreateObjectStorageKey made during ensureAtConfigure).
	testTagCount := 0
	for _, k := range fake.keys {
		if k.Label == keyLabelFor("test-tag") {
			testTagCount++
		}
	}
	if testTagCount != 1 {
		t.Errorf("expected exactly 1 test-tag key after reap+create, got %d", testTagCount)
	}
}

func TestBucketMaybeCleanupDeletesKeyAndBucket(t *testing.T) {
	ctx := context.Background()
	fake := newFakeBucketClient()
	m := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fake, slog.Default())
	if _, err := m.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	keyID := m.keyID

	m.maybeCleanupBucket(ctx)

	if _, ok := fake.keys[keyID]; ok {
		t.Errorf("scoped key %d still present after cleanup", keyID)
	}
	if _, ok := fake.buckets[bucketKey(testBucketRegion, "fjb-cache-test-tag")]; ok {
		t.Errorf("bucket still present after cleanup")
	}
	if m.keyID != 0 {
		t.Errorf("m.keyID should be reset to 0, got %d", m.keyID)
	}
}

func TestBucketMaybeCleanupNonEmptyBucketIsNotFatal(t *testing.T) {
	// Linode rejects DELETE on a non-empty bucket with a 400. The cache
	// pattern is to keep the cached layers across deployments, so we
	// log and move on instead of failing the teardown.
	ctx := context.Background()
	fake := newFakeBucketClient()
	m := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fake, slog.Default())
	if _, err := m.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	fake.deleteBucketErr = &linodego.Error{Code: 400, Message: "bucket not empty"}

	// Should not panic, should not bubble.
	m.maybeCleanupBucket(ctx)

	if fake.deleteKeyCalls != 1 {
		t.Errorf("key delete should still run when bucket delete fails (got %d calls)", fake.deleteKeyCalls)
	}
}

func TestBucketLabelFor(t *testing.T) {
	cases := []struct {
		name     string
		tag      string
		mustHave string
	}{
		{name: "lowercase tag", tag: "deploy-a", mustHave: "fjb-cache-deploy-a"},
		{name: "uppercase tag lowercased", tag: "DeployA", mustHave: "fjb-cache-deploya"},
		{name: "underscore replaced", tag: "deploy_a", mustHave: "fjb-cache-deploy-a"},
		{name: "long tag truncated", tag: strings.Repeat("a", 80), mustHave: "fjb-cache-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bucketLabelFor(c.tag)
			if len(got) < 3 || len(got) > 63 {
				t.Errorf("len(%q) = %d, want 3-63", got, len(got))
			}
			if !strings.Contains(got, c.mustHave) {
				t.Errorf("got %q, want substring %q", got, c.mustHave)
			}
			if strings.ContainsAny(got, "_.ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
				t.Errorf("bucket label %q contains forbidden chars (uppercase/underscore/dot)", got)
			}
			if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
				t.Errorf("bucket label %q has leading/trailing hyphen", got)
			}
		})
	}
}

func TestBucketLabelForLongTagsDontCollide(t *testing.T) {
	a := bucketLabelFor(strings.Repeat("a", 80))
	b := bucketLabelFor(strings.Repeat("b", 80))
	if a == b {
		t.Errorf("two distinct 80-char tags collided: both → %q", a)
	}
}

// isNotFound recognises the 404 error shape ensureAtConfigure expects
// during the bucket find-or-create.
func TestIsNotFoundRecognizesLinodego404(t *testing.T) {
	if !isNotFound(&linodego.Error{Code: 404}) {
		t.Errorf("isNotFound should recognise *linodego.Error{Code:404}")
	}
	if !isNotFound(errors.New("[404] string-shaped not found")) {
		t.Errorf("isNotFound should recognise string-shaped [404]")
	}
	if isNotFound(&linodego.Error{Code: 500}) {
		t.Errorf("isNotFound should not match 500")
	}
	if isNotFound(nil) {
		t.Errorf("isNotFound(nil) should be false")
	}
}

func TestBucketClientInterfaceCompiles(_ *testing.T) {
	var _ bucketClient = (*linodego.Client)(nil)
}
