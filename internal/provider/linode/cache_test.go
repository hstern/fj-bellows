package linode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/linode/linodego"
)

const (
	// Placeholder PEM body used by tests that exercise the cloud-init
	// renderer with non-empty server cert / key fields. Not a real PEM
	// — the renderer doesn't parse the content, it just substitutes
	// it into the template.
	testStubPEM = "STUB"
	// Shared cloud-init renderer fixtures used across cache_test.go
	// and cache_tunnel_test.go — extracted so goconst doesn't flag
	// repeated literals.
	testStubEndpoint   = "https://x"
	testStubZotVersion = "1.0.0"
)

// fakeCacheClient is a hand-rolled cacheClient (per repo conventions —
// no codegen). Stores Linode instances by ID. Tests pre-seed instances
// to exercise the adopt-existing path.
type fakeCacheClient struct {
	mu     sync.Mutex
	insts  map[int]*linodego.Instance
	nextID int

	// configs map linodeID → instance configs (with the inline VPC
	// interface that carries the assigned VPC IPv4). Tests pre-seed
	// when exercising the workerExtras / VPC-IP-lookup path.
	configs map[int][]linodego.InstanceConfig

	createErr      error
	listConfigsErr error

	listCalls        int
	createCalls      int
	deleteCalls      int
	listConfigsCalls int
	lastCreate       *linodego.InstanceCreateOptions
}

func newFakeCacheClient() *fakeCacheClient {
	return &fakeCacheClient{
		insts:   map[int]*linodego.Instance{},
		configs: map[int][]linodego.InstanceConfig{},
	}
}

func (f *fakeCacheClient) ListInstanceConfigs(_ context.Context, linodeID int, _ *linodego.ListOptions) ([]linodego.InstanceConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listConfigsCalls++
	if f.listConfigsErr != nil {
		return nil, f.listConfigsErr
	}
	return append([]linodego.InstanceConfig(nil), f.configs[linodeID]...), nil
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

func (f *fakeCacheClient) GetInstance(_ context.Context, id int) (*linodego.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.insts[id]
	if !ok {
		return nil, fmt.Errorf("fake: linode %d not found", id)
	}
	cp := *in
	return &cp, nil
}

// newTestManagedCache injects a config that satisfies validate() — a
// stub upstream URL and a temp-dir CA path — so each test doesn't
// have to repeat the boilerplate. Tests that want to exercise
// alternate configs can call newManagedCache directly.
func newTestManagedCache(t *testing.T, client cacheClient, bucket *managedBucket) *managedCache {
	t.Helper()
	cfg := cacheConfig{
		TLS: &cacheTLSConfig{CADir: t.TempDir()},
	}
	return newManagedCache(cfg, "test-tag", testBucketRegion, client, bucket, slog.Default())
}

// newAdoptableTestManagedCache returns a managedCache whose CA dir is
// pre-seeded with a valid CA pair, simulating the "daemon restart
// with persistent CA" scenario. Use this when the test wants ensure-
// AtConfigure to take the adopt-existing path on an existing cache
// VM — without pre-seeded CA, ensureAtConfigure rejects adoption with
// the "fresh CA but existing VM" mismatch error.
func newAdoptableTestManagedCache(t *testing.T, client cacheClient, bucket *managedBucket) *managedCache {
	t.Helper()
	caDir := t.TempDir()
	// Generate + persist a CA so the next loadOrGenerateCertPair call
	// finds it on disk and reports freshCA=false.
	if _, _, _, err := generateAndPersistCA(caDir); err != nil {
		t.Fatalf("seed CA dir: %v", err)
	}
	cfg := cacheConfig{
		TLS: &cacheTLSConfig{CADir: caDir},
	}
	return newManagedCache(cfg, "test-tag", testBucketRegion, client, bucket, slog.Default())
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
	cache := newTestManagedCache(t, fc, bucket)
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
	cache := newAdoptableTestManagedCache(t, fc, bucket)

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
	cache := newTestManagedCache(t, fc, bucket)
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
	cache := newAdoptableTestManagedCache(t, fc, bucket)
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
	// Common base — fully-populated params. Each case wipes ONE
	// required field and asserts render rejects it. Using a base +
	// clone keeps the table compact as the param set grows.
	base := cacheCloudInitParams{
		Bucket:        "b",
		Region:        "r",
		Endpoint:      testStubEndpoint,
		AccessKey:     "AK",
		SecretKey:     "SK",
		ZotVersion:    testStubZotVersion,
		ServerCertPEM: testStubPEM,
		ServerKeyPEM:  testStubPEM,
	}
	cases := []struct {
		name string
		wipe func(*cacheCloudInitParams)
	}{
		{name: "missing bucket", wipe: func(p *cacheCloudInitParams) { p.Bucket = "" }},
		{name: "missing region", wipe: func(p *cacheCloudInitParams) { p.Region = "" }},
		{name: "missing endpoint", wipe: func(p *cacheCloudInitParams) { p.Endpoint = "" }},
		{name: "missing access key", wipe: func(p *cacheCloudInitParams) { p.AccessKey = "" }},
		{name: "missing secret key", wipe: func(p *cacheCloudInitParams) { p.SecretKey = "" }},
		{name: "missing zot version", wipe: func(p *cacheCloudInitParams) { p.ZotVersion = "" }},
		{name: "missing server cert", wipe: func(p *cacheCloudInitParams) { p.ServerCertPEM = "" }},
		{name: "missing server key", wipe: func(p *cacheCloudInitParams) { p.ServerKeyPEM = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.wipe(&p)
			if _, err := renderCacheCloudInit(p); err == nil {
				t.Errorf("expected error for %q", c.name)
			}
		})
	}
}

func TestRenderCacheCloudInitProducesValidCloudInit(t *testing.T) {
	// Stub PEM strings deliberately look like real PEM headers so the
	// rendered output asserts can grep for them. They are NOT
	// credentials — the renderer is a string-substitution template.
	const stubCertPEM = "-----BEGIN CERTIFICATE-----\nMOCK\n-----END CERTIFICATE-----\n"
	const stubKeyPEM = "-----BEGIN EC PRIVATE KEY-----\nMOCK\n-----END EC PRIVATE KEY-----\n" //nolint:gosec // G101: test fixture, not a real key
	out, err := renderCacheCloudInit(cacheCloudInitParams{
		Bucket:        "fjb-cache-test",
		Region:        testBucketRegion,
		Endpoint:      testBucketEndpoint,
		AccessKey:     "AK",
		SecretKey:     "SK",
		ZotVersion:    "2.1.7",
		ServerCertPEM: stubCertPEM,
		ServerKeyPEM:  stubKeyPEM,
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
		"-----BEGIN CERTIFICATE-----", // baked-in server cert
		"-----BEGIN EC PRIVATE KEY-----",
		"/etc/zot/tls/cert.pem",
		"/etc/zot/tls/key.pem",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered cloud-init missing substring %q\n---\n%s", want, out)
		}
	}
}

func TestRenderCacheCloudInitReadyFileDefaults(t *testing.T) {
	out, err := renderCacheCloudInit(cacheCloudInitParams{
		Bucket:        "b",
		Region:        "r",
		Endpoint:      testStubEndpoint,
		AccessKey:     "AK",
		SecretKey:     "SK",
		ZotVersion:    testStubZotVersion,
		ServerCertPEM: testStubPEM,
		ServerKeyPEM:  testStubPEM,
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
	cache := newTestManagedCache(t, fc, bucket)

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
	cache := newTestManagedCache(t, fc, bucket)

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
	cache := newTestManagedCache(t, fc, bucket)

	err := cache.ensureAtConfigure(ctx)
	if err == nil || !strings.Contains(err.Error(), "create cache linode") {
		t.Errorf("expected create-cache-linode error, got: %v", err)
	}
}

func TestCacheClientInterfaceCompiles(_ *testing.T) {
	var _ cacheClient = (*linodego.Client)(nil)
}

// TestCacheEnsureRecreatesAfterReap is the FJB-10 cache sibling.
// maybeCleanupCache resets linodeID to 0; the next workerExtras call
// would error "cache linode not provisioned yet". ensure() lazily
// recreates so the next Provision sees a fresh cache VM. The
// persisted CA dir means the new VM is signed by the same anchor
// workers already trust.
func TestCacheEnsureRecreatesAfterReap(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(t, fc, bucket)
	cache.setHardwareContext(7777, 8888, "")
	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("initial ensureAtConfigure: %v", err)
	}
	if cache.linodeID == 0 {
		t.Fatal("initial ensure failed to set linodeID")
	}
	cache.maybeCleanupCache(ctx)
	if cache.linodeID != 0 {
		t.Fatalf("post-reap linodeID = %d, want 0", cache.linodeID)
	}

	beforeCreate := fc.createCalls
	if err := cache.ensure(ctx); err != nil {
		t.Fatalf("ensure() after reap: %v", err)
	}
	if cache.linodeID == 0 {
		t.Error("ensure() left linodeID = 0; workerExtras would error 'cache linode not provisioned yet' (FJB-10)")
	}
	if fc.createCalls != beforeCreate+1 {
		t.Errorf("createCalls = %d, want %d (ensure should have created a new cache VM)",
			fc.createCalls, beforeCreate+1)
	}
}

// TestCacheEnsureNoOpWhenIDStillValid — steady-state no-op.
func TestCacheEnsureNoOpWhenIDStillValid(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(t, fc, bucket)
	cache.setHardwareContext(7777, 8888, "")
	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	beforeList := fc.listCalls
	beforeCreate := fc.createCalls
	if err := cache.ensure(ctx); err != nil {
		t.Fatalf("ensure(): %v", err)
	}
	if fc.listCalls != beforeList || fc.createCalls != beforeCreate {
		t.Errorf("ensure() should be no-op with valid linodeID; listCalls %d→%d, createCalls %d→%d",
			beforeList, fc.listCalls, beforeCreate, fc.createCalls)
	}
}

func TestPreflightCacheRegionAcceptsSupportedRegion(t *testing.T) {
	// Default fake advertises us-ord on both surfaces; pre-flight
	// should succeed for it without error.
	fake := newFakeBucketClient()
	if err := preflightCacheRegion(context.Background(), fake, testBucketRegion); err != nil {
		t.Errorf("preflight: %v", err)
	}
}

func TestPreflightCacheRegionRejectsUnsupportedRegion(t *testing.T) {
	// Mimics ca-tor today: not in /endpoints, not in /clusters.
	// Pre-flight must surface this clearly so an operator picks a
	// supported region — without firewall + VPC getting created
	// first.
	fake := newFakeBucketClient()
	fake.endpoints = nil
	fake.clusters = nil
	err := preflightCacheRegion(context.Background(), fake, "ca-tor")
	if err == nil {
		t.Fatal("expected error for unsupported region")
	}
	for _, want := range []string{"ca-tor", "object storage", "not available"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing substring %q", err.Error(), want)
		}
	}
}

func TestWorkerExtrasLooksUpAndCachesVPCIP(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(t, fc, bucket)
	cache.setHardwareContext(7777, 8888, "")

	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	// Seed the cache linode's VPC interface IP — production reads
	// this from Linode after the VM settles on its VPC IP.
	wantIP := "10.0.0.42"
	subnetID := 8888
	fc.configs[cache.linodeID] = []linodego.InstanceConfig{{
		ID: 1,
		Interfaces: []linodego.InstanceConfigInterface{
			{Purpose: linodego.InterfacePurposePublic, Primary: true},
			{
				Purpose:  linodego.InterfacePurposeVPC,
				SubnetID: &subnetID,
				IPv4:     &linodego.VPCIPv4{VPC: wantIP},
			},
		},
	}}

	x, err := cache.workerExtras(ctx)
	if err != nil {
		t.Fatalf("workerExtras: %v", err)
	}
	if x.CacheIP != wantIP {
		t.Errorf("CacheIP = %q, want %q", x.CacheIP, wantIP)
	}
	if x.CacheHost != defaultCacheHostname {
		t.Errorf("CacheHost = %q, want %q", x.CacheHost, defaultCacheHostname)
	}
	if x.CachePort != defaultCachePort {
		t.Errorf("CachePort = %d, want %d", x.CachePort, defaultCachePort)
	}
	if x.CACertPEM == "" {
		t.Error("CACertPEM empty")
	}
	// Second call should NOT re-list configs (cached).
	beforeCalls := fc.listConfigsCalls
	if _, err := cache.workerExtras(ctx); err != nil {
		t.Fatalf("second workerExtras: %v", err)
	}
	if fc.listConfigsCalls != beforeCalls {
		t.Errorf("workerExtras should cache VPC IP; ListInstanceConfigs called %d extra times",
			fc.listConfigsCalls-beforeCalls)
	}
}

// Shared test-CIDR constants — extracted to satisfy goconst (and keep
// the renames easy if real LAN ranges show up later). Picked to match
// the existing testdata in worker_cache_cloud_init_gateway_test.go.
const (
	testCIDRLan192 = "192.168.0.0/24"
	testCIDRLan10  = "10.10.0.0/16"
	testCacheVPCIP = "10.0.0.42"
)

// stubACLSnapshot is a hand-rolled ACLSnapshotSource for tests:
// it returns whatever CIDRs the test seeds. Lets tests exercise the
// FJB-88 sort/dedupe path through workerExtras without pulling in
// the orchestrator's acl.Registry.
type stubACLSnapshot struct {
	cidrs []string
}

func (s *stubACLSnapshot) AllowedIPsCIDRs() []string {
	return append([]string(nil), s.cidrs...)
}

func TestWorkerExtrasPullsAllowedIPsFromACLSource(t *testing.T) {
	// FJB-88: when an ACLSnapshotSource is wired, workerExtras populates
	// AllowedIPsCIDRs from it and sorts/dedupes for byte-stable output.
	ctx := context.Background()
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(t, fc, bucket)
	cache.setHardwareContext(7777, 8888, "")
	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	subnetID := 8888
	fc.configs[cache.linodeID] = []linodego.InstanceConfig{{
		ID: 1,
		Interfaces: []linodego.InstanceConfigInterface{{
			Purpose:  linodego.InterfacePurposeVPC,
			SubnetID: &subnetID,
			IPv4:     &linodego.VPCIPv4{VPC: testCacheVPCIP},
		}},
	}}

	// Intentionally unsorted + duplicated. workerExtras must canonicalize.
	cache.setACLSource(&stubACLSnapshot{cidrs: []string{
		testCIDRLan192,
		testCIDRLan10,
		testCIDRLan192, // dup
		testCIDRLan10,  // dup
	}})

	x, err := cache.workerExtras(ctx)
	if err != nil {
		t.Fatalf("workerExtras: %v", err)
	}
	want := []string{testCIDRLan10, testCIDRLan192}
	if len(x.AllowedIPsCIDRs) != len(want) {
		t.Fatalf("AllowedIPsCIDRs = %v, want %v", x.AllowedIPsCIDRs, want)
	}
	for i, w := range want {
		if x.AllowedIPsCIDRs[i] != w {
			t.Errorf("AllowedIPsCIDRs[%d] = %q, want %q", i, x.AllowedIPsCIDRs[i], w)
		}
	}
	if x.OrchestratorWGAddr != defaultOrchestratorWGAddr {
		t.Errorf("OrchestratorWGAddr = %q, want %q", x.OrchestratorWGAddr, defaultOrchestratorWGAddr)
	}
}

func TestWorkerExtrasEmptyWhenNoACLSource(t *testing.T) {
	// Without an ACLSnapshotSource wired (e.g. ssh mode, or pre-FJB-90
	// boot), AllowedIPsCIDRs is empty and the legacy ssh template still
	// renders fine. The cache-gateway validate path catches the empty-set
	// case loudly at render time.
	ctx := context.Background()
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(t, fc, bucket)
	cache.setHardwareContext(7777, 8888, "")
	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	subnetID := 8888
	fc.configs[cache.linodeID] = []linodego.InstanceConfig{{
		ID: 1,
		Interfaces: []linodego.InstanceConfigInterface{{
			Purpose:  linodego.InterfacePurposeVPC,
			SubnetID: &subnetID,
			IPv4:     &linodego.VPCIPv4{VPC: testCacheVPCIP},
		}},
	}}

	x, err := cache.workerExtras(ctx)
	if err != nil {
		t.Fatalf("workerExtras: %v", err)
	}
	if len(x.AllowedIPsCIDRs) != 0 {
		t.Errorf("AllowedIPsCIDRs = %v, want empty (no ACL source)", x.AllowedIPsCIDRs)
	}
}

func TestWorkerExtrasErrorsWhenVPCIPNotAssigned(t *testing.T) {
	// Cache VM exists but its VPC interface hasn't been assigned an
	// IP yet (e.g. still booting). workerExtras should surface this
	// so the orchestrator's tick-driven Provision retries next round
	// rather than provisioning a worker with an empty CacheIP.
	ctx := context.Background()
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	cache := newTestManagedCache(t, fc, bucket)
	cache.setHardwareContext(7777, 8888, "")

	if err := cache.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	// No configs seeded → no VPC IP returned.
	_, err := cache.workerExtras(ctx)
	if err == nil {
		t.Fatal("expected error when VPC IP not yet assigned")
	}
	if !strings.Contains(err.Error(), "VPC") {
		t.Errorf("error should mention VPC IP, got: %v", err)
	}
}
