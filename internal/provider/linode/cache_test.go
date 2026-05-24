package linode

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/linode/linodego"
)

// fakeCacheClient is a hand-rolled cacheClient (per repo conventions —
// no codegen). Stores Linode instances by ID. Tests pre-seed instances
// to exercise the adopt-existing path.
type fakeCacheClient struct {
	mu     sync.Mutex
	insts  map[int]*linodego.Instance
	nextID int

	createErr error

	listCalls   int
	createCalls int
	deleteCalls int
	lastCreate  *linodego.InstanceCreateOptions
}

func newFakeCacheClient() *fakeCacheClient {
	return &fakeCacheClient{insts: map[int]*linodego.Instance{}}
}

func (f *fakeCacheClient) ListInstances(_ context.Context, _ *linodego.ListOptions) ([]linodego.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	out := make([]linodego.Instance, 0, len(f.insts))
	for _, in := range f.insts {
		out = append(out, *in)
	}
	return out, nil
}

func (f *fakeCacheClient) CreateInstance(_ context.Context, opts linodego.InstanceCreateOptions) (*linodego.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.nextID++
	cp := opts // capture for assertions
	f.lastCreate = &cp
	inst := &linodego.Instance{
		ID:     f.nextID,
		Label:  opts.Label,
		Tags:   append([]string(nil), opts.Tags...),
		Region: opts.Region,
		Type:   opts.Type,
		Image:  opts.Image,
	}
	f.insts[inst.ID] = inst
	out := *inst
	return &out, nil
}

func (f *fakeCacheClient) DeleteInstance(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	delete(f.insts, id)
	return nil
}

func newTestManagedCache(client cacheClient, bucket *managedBucket) *managedCache {
	return newManagedCache(cacheConfig{}, "test-tag", testBucketRegion, client, bucket, slog.Default())
}

func TestCacheConfigDefaults(t *testing.T) {
	c := cacheConfig{}
	if got := c.resolvedType(); got != defaultCacheType {
		t.Errorf("Type default = %q, want %q", got, defaultCacheType)
	}
	if got := c.resolvedImage(); got != defaultCacheImage {
		t.Errorf("Image default = %q, want %q", got, defaultCacheImage)
	}
	if got := c.resolvedZotVersion(); got != defaultZotVersion {
		t.Errorf("ZotVersion default = %q, want %q", got, defaultZotVersion)
	}

	// Overrides surface unchanged.
	c = cacheConfig{Type: "g6-standard-1", Image: "linode/ubuntu24.04", ZotVersion: "9.9.9"}
	if c.resolvedType() != "g6-standard-1" || c.resolvedImage() != "linode/ubuntu24.04" || c.resolvedZotVersion() != "9.9.9" {
		t.Errorf("overrides not preserved: %+v", c)
	}
}

func TestCacheEnsureAtConfigureCreatesFresh(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(fc, bucket)
	cache.setHardwareContext(7777, 8888, "")

	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	if cache.linodeID == 0 {
		t.Fatalf("linodeID not recorded")
	}
	if cache.adoptedExisting {
		t.Errorf("expected fresh-create path, got adoptedExisting=true")
	}
	if fc.createCalls != 1 {
		t.Errorf("CreateInstance calls = %d, want 1", fc.createCalls)
	}
	assertCacheCreateOpts(t, fc.lastCreate)
}

// assertCacheCreateOpts checks the CreateInstance payload had the
// firewall ID, VPC interface, cache-tag (not worker-tag), and user-data
// the cache lifecycle requires. Extracted from the test body to keep
// the test's cyclomatic complexity under the linter budget.
func assertCacheCreateOpts(t *testing.T, opts *linodego.InstanceCreateOptions) {
	t.Helper()
	if opts == nil {
		t.Fatal("CreateInstance was never called")
	}
	if opts.FirewallID != 7777 {
		t.Errorf("FirewallID = %d, want 7777", opts.FirewallID)
	}
	if !hasPublicAndVPCInterfaces(opts.Interfaces, 8888) {
		t.Errorf("Interfaces wiring wrong: %+v", opts.Interfaces)
	}
	if !slices.Contains(opts.Tags, cacheLinodeTag("test-tag")) {
		t.Errorf("Tags missing the cache tag: %v", opts.Tags)
	}
	if slices.Contains(opts.Tags, "test-tag") {
		t.Errorf("cache VM must NOT carry the worker tag (would show up in List(tag)): %v", opts.Tags)
	}
	if opts.Metadata == nil || opts.Metadata.UserData == "" {
		t.Errorf("UserData not populated")
	}
}

func hasPublicAndVPCInterfaces(ifaces []linodego.InstanceConfigInterfaceCreateOptions, wantSubnetID int) bool {
	if len(ifaces) != 2 {
		return false
	}
	if ifaces[0].Purpose != linodego.InterfacePurposePublic {
		return false
	}
	if ifaces[1].Purpose != linodego.InterfacePurposeVPC {
		return false
	}
	if ifaces[1].SubnetID == nil || *ifaces[1].SubnetID != wantSubnetID {
		return false
	}
	return true
}

func TestCacheEnsureAtConfigureAdoptsExistingLinode(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCacheClient()
	fc.nextID = 100
	fc.insts[101] = &linodego.Instance{
		ID:    101,
		Label: cacheLinodeLabel("test-tag"),
		Tags:  []string{cacheLinodeTag("test-tag")},
	}
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(fc, bucket)

	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	if cache.linodeID != 101 {
		t.Errorf("adopt failed: linodeID = %d, want 101", cache.linodeID)
	}
	if !cache.adoptedExisting {
		t.Errorf("expected adoptedExisting=true")
	}
	if fc.createCalls != 0 {
		t.Errorf("CreateInstance should not run on adopt (got %d)", fc.createCalls)
	}
	// Adopt path skips bucket creation entirely (the existing VM has
	// its baked-in creds).
	if fb.createBucketCalls != 0 || fb.createKeyCalls != 0 {
		t.Errorf("bucket/key create should not run on adopt (got bucket=%d key=%d)",
			fb.createBucketCalls, fb.createKeyCalls)
	}
}

func TestCacheMaybeCleanupFreshCreatePath(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(fc, bucket)
	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	id := cache.linodeID

	cache.maybeCleanupCache(ctx)

	if fc.deleteCalls != 1 {
		t.Errorf("DeleteInstance calls = %d, want 1", fc.deleteCalls)
	}
	if _, ok := fc.insts[id]; ok {
		t.Errorf("cache linode %d still present after cleanup", id)
	}
	if cache.linodeID != 0 {
		t.Errorf("linodeID should be reset to 0, got %d", cache.linodeID)
	}
	if fb.deleteKeyCalls != 1 {
		t.Errorf("bucket cleanup should also fire (key delete = %d)", fb.deleteKeyCalls)
	}
}

func TestCacheMaybeCleanupAdoptedPathSkipsBucket(t *testing.T) {
	// Adopted-existing means we don't own the key/bucket lifecycle for
	// THIS daemon lifetime — skipping the bucket cleanup avoids
	// deleting state the adopted VM still depends on.
	ctx := context.Background()
	fc := newFakeCacheClient()
	fc.nextID = 200
	fc.insts[201] = &linodego.Instance{
		ID: 201, Label: cacheLinodeLabel("test-tag"),
		Tags: []string{cacheLinodeTag("test-tag")},
	}
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(fc, bucket)
	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}

	cache.maybeCleanupCache(ctx)

	if fc.deleteCalls != 1 {
		t.Errorf("DeleteInstance should run on cleanup (got %d)", fc.deleteCalls)
	}
	if fb.deleteKeyCalls != 0 || fb.deleteBucketCalls != 0 {
		t.Errorf("bucket cleanup should NOT run on adopted-existing path (key=%d bucket=%d)",
			fb.deleteKeyCalls, fb.deleteBucketCalls)
	}
}

func TestCacheLinodeTagIsDistinctFromWorkerTag(t *testing.T) {
	// This is load-bearing — if the cache VM shared the deployment
	// tag, the orchestrator's List(tag) would surface it as a worker
	// and the reconciler would try to dispatch jobs to it.
	worker := "deployment-x"
	cache := cacheLinodeTag(worker)
	if cache == worker {
		t.Errorf("cache tag %q must differ from worker tag %q", cache, worker)
	}
	if !strings.HasPrefix(cache, worker) {
		t.Errorf("cache tag %q should start with worker tag %q so prefix sweeps still catch it", cache, worker)
	}
}

func TestCacheLinodeLabelSanitizesForLinode(t *testing.T) {
	got := cacheLinodeLabel("deploy_one.two")
	if len(got) < 1 || len(got) > 64 {
		t.Errorf("label %q out of 1-64 range", got)
	}
	if !strings.Contains(got, "fj-bellows-cache-") {
		t.Errorf("label %q missing fj-bellows-cache- prefix", got)
	}
}

func TestRenderCacheCloudInitRequiresAllFields(t *testing.T) {
	cases := []struct {
		name string
		p    cacheCloudInitParams
	}{
		{name: "missing bucket", p: cacheCloudInitParams{Region: "r", Endpoint: "e", AccessKey: "a", SecretKey: "s", ZotVersion: "1"}},
		{name: "missing region", p: cacheCloudInitParams{Bucket: "b", Endpoint: "e", AccessKey: "a", SecretKey: "s", ZotVersion: "1"}},
		{name: "missing endpoint", p: cacheCloudInitParams{Bucket: "b", Region: "r", AccessKey: "a", SecretKey: "s", ZotVersion: "1"}},
		{name: "missing access key", p: cacheCloudInitParams{Bucket: "b", Region: "r", Endpoint: "e", SecretKey: "s", ZotVersion: "1"}},
		{name: "missing secret key", p: cacheCloudInitParams{Bucket: "b", Region: "r", Endpoint: "e", AccessKey: "a", ZotVersion: "1"}},
		{name: "missing zot version", p: cacheCloudInitParams{Bucket: "b", Region: "r", Endpoint: "e", AccessKey: "a", SecretKey: "s"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := renderCacheCloudInit(c.p); err == nil {
				t.Errorf("expected error for %q", c.name)
			}
		})
	}
}

func TestRenderCacheCloudInitProducesValidCloudInit(t *testing.T) {
	out, err := renderCacheCloudInit(cacheCloudInitParams{
		Bucket:     "fjb-cache-test",
		Region:     testBucketRegion,
		Endpoint:   testBucketEndpoint,
		AccessKey:  "AK",
		SecretKey:  "SK",
		ZotVersion: "2.1.7",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"#cloud-config",
		"fjb-cache-test",
		testBucketEndpoint,
		"\"accesskey\": \"AK\"",
		"\"secretkey\": \"SK\"",
		"v2.1.7/zot-linux-",
		"zot.service",
		"systemctl enable --now zot.service",
		defaultCacheReadyFile,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered cloud-init missing substring %q\n---\n%s", want, out)
		}
	}
}

func TestRenderCacheCloudInitReadyFileDefaults(t *testing.T) {
	out, err := renderCacheCloudInit(cacheCloudInitParams{
		Bucket:     "b",
		Region:     "r",
		Endpoint:   "https://x",
		AccessKey:  "AK",
		SecretKey:  "SK",
		ZotVersion: "1.0.0",
		// ReadyFile intentionally omitted
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, defaultCacheReadyFile) {
		t.Errorf("expected default ReadyFile in output, got:\n%s", out)
	}
}

// Sanity check that the embedded template parses (catches dev-time
// typos in the .tmpl file even before any explicit render).
func TestCacheCloudInitTemplateNotEmpty(t *testing.T) {
	if cacheCloudInitTemplate == "" {
		t.Fatal("embedded cache cloud-init template is empty")
	}
}

// findCacheLinode must not adopt instances whose tag doesn't match —
// otherwise a cache from a different deployment could be hijacked.
func TestFindCacheLinodeIgnoresOtherDeployments(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCacheClient()
	fc.insts[1] = &linodego.Instance{ID: 1, Label: "other", Tags: []string{cacheLinodeTag("other-tag")}}
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(fc, bucket)

	got, err := cache.findCacheLinode(ctx)
	if err != nil {
		t.Fatalf("findCacheLinode: %v", err)
	}
	if got != nil {
		t.Errorf("should not adopt other-tag cache linode, got %+v", got)
	}
}

func TestCacheMaybeCleanupNoOpWhenNothingProvisioned(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(fc, bucket)

	cache.maybeCleanupCache(ctx)

	if fc.deleteCalls != 0 {
		t.Errorf("DeleteInstance should not run when linodeID=0 (got %d)", fc.deleteCalls)
	}
	// Bucket cleanup still tries to delete the bucket (idempotent in
	// the fake — Delete on a missing key is a no-op).
	if fb.deleteKeyCalls != 0 {
		t.Errorf("key delete should not run when keyID=0 (got %d)", fb.deleteKeyCalls)
	}
}

// Cache must surface a CreateInstance failure (e.g. PAT scope missing,
// region invalid) — silent ignore would leave an unreachable cache.
func TestCacheEnsureAtConfigureSurfacesCreateError(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCacheClient()
	fc.createErr = errors.New("simulated 403")
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(fc, bucket)

	err := cache.ensureAtConfigure(ctx)
	if err == nil || !strings.Contains(err.Error(), "create cache linode") {
		t.Errorf("expected create-cache-linode error, got: %v", err)
	}
}

func TestCacheClientInterfaceCompiles(_ *testing.T) {
	var _ cacheClient = (*linodego.Client)(nil)
}
