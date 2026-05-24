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

// Test fixtures hoisted to constants — referenced enough that the linter
// (goconst, min-occurrences=4) complains otherwise.
const (
	testSubnetCache   = "cache"
	testSubnetWorkers = "workers"
	testCIDRCache     = "100.64.0.0/24"
	testCIDRWorkers   = "100.64.1.0/24"
)

// fakeVPCClient is a hand-rolled vpcClient (per repo conventions — no
// codegen). Stores VPCs in a map keyed by ID; subnets live inline on each
// VPC. Tests can pre-seed `vpcs` to exercise the find-or-create path and
// inject `subnetLinodes` to exercise maybeCleanupVPC's "has linodes" branch.
type fakeVPCClient struct {
	mu     sync.Mutex
	vpcs   map[int]*linodego.VPC
	nextID int

	// subnetLinodes maps subnet ID → slice of attached linodes returned by
	// GetVPC. Tests inject entries here to keep maybeCleanupVPC from
	// deleting; cleared once a subnet would be torn down.
	subnetLinodes map[int][]linodego.VPCSubnetLinode

	listCalls         int
	getCalls          int
	createCalls       int
	createSubnetCalls int
	deleteCalls       int
	deleteSubnetCalls int
}

func newFakeVPCClient() *fakeVPCClient {
	return &fakeVPCClient{
		vpcs:          map[int]*linodego.VPC{},
		subnetLinodes: map[int][]linodego.VPCSubnetLinode{},
	}
}

func (f *fakeVPCClient) ListVPCs(_ context.Context, _ *linodego.ListOptions) ([]linodego.VPC, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	out := make([]linodego.VPC, 0, len(f.vpcs))
	for _, v := range f.vpcs {
		out = append(out, *v)
	}
	return out, nil
}

func (f *fakeVPCClient) GetVPC(_ context.Context, id int) (*linodego.VPC, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	v, ok := f.vpcs[id]
	if !ok {
		return nil, errors.New("not found")
	}
	// Copy and populate Linodes per the test's seeded state. The real API
	// returns subnets with .Linodes populated; List returns subnets but
	// not always with attachments — maybeCleanupVPC re-Gets for exactly
	// this reason, so mirror that contract here.
	cp := *v
	subs := make([]linodego.VPCSubnet, len(v.Subnets))
	for i := range v.Subnets {
		subs[i] = v.Subnets[i]
		subs[i].Linodes = append([]linodego.VPCSubnetLinode(nil), f.subnetLinodes[v.Subnets[i].ID]...)
	}
	cp.Subnets = subs
	return &cp, nil
}

func (f *fakeVPCClient) CreateVPC(_ context.Context, opts linodego.VPCCreateOptions) (*linodego.VPC, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.nextID++
	v := &linodego.VPC{
		ID:          f.nextID,
		Label:       opts.Label,
		Description: opts.Description,
		Region:      opts.Region,
	}
	for _, s := range opts.Subnets {
		f.nextID++
		v.Subnets = append(v.Subnets, linodego.VPCSubnet{
			ID:    f.nextID,
			Label: s.Label,
			IPv4:  s.IPv4,
		})
	}
	f.vpcs[v.ID] = v
	cp := *v
	return &cp, nil
}

func (f *fakeVPCClient) DeleteVPC(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	delete(f.vpcs, id)
	return nil
}

func (f *fakeVPCClient) CreateVPCSubnet(_ context.Context, opts linodego.VPCSubnetCreateOptions, vpcID int) (*linodego.VPCSubnet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createSubnetCalls++
	v, ok := f.vpcs[vpcID]
	if !ok {
		return nil, errors.New("vpc not found")
	}
	f.nextID++
	s := linodego.VPCSubnet{
		ID:    f.nextID,
		Label: opts.Label,
		IPv4:  opts.IPv4,
	}
	v.Subnets = append(v.Subnets, s)
	cp := s
	return &cp, nil
}

func (f *fakeVPCClient) DeleteVPCSubnet(_ context.Context, vpcID, subnetID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteSubnetCalls++
	v, ok := f.vpcs[vpcID]
	if !ok {
		return nil
	}
	kept := v.Subnets[:0]
	for _, s := range v.Subnets {
		if s.ID != subnetID {
			kept = append(kept, s)
		}
	}
	v.Subnets = kept
	delete(f.subnetLinodes, subnetID)
	return nil
}

func TestVPCConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     vpcConfig
		wantErr string // substring; "" = expect no error
	}{
		{
			name:    "empty subnets",
			cfg:     vpcConfig{},
			wantErr: "subnets is required",
		},
		{
			name: "subnet missing ipv4",
			cfg: vpcConfig{Subnets: map[string]subnetConfig{
				testSubnetCache: {},
			}},
			wantErr: "ipv4 is required",
		},
		{
			name: "subnet bad CIDR",
			cfg: vpcConfig{Subnets: map[string]subnetConfig{
				testSubnetCache: {IPv4: "not-a-cidr"},
			}},
			wantErr: "is not a CIDR",
		},
		{
			name: "worker_subnet not declared",
			cfg: vpcConfig{
				Subnets:      map[string]subnetConfig{testSubnetCache: {IPv4: testCIDRCache}},
				WorkerSubnet: "missing",
			},
			wantErr: "is not declared",
		},
		{
			name: "valid single subnet",
			cfg: vpcConfig{Subnets: map[string]subnetConfig{
				testSubnetCache: {IPv4: testCIDRCache},
			}},
		},
		{
			name: "valid with worker_subnet override",
			cfg: vpcConfig{
				Subnets: map[string]subnetConfig{
					testSubnetCache:   {IPv4: testCIDRCache},
					testSubnetWorkers: {IPv4: testCIDRWorkers},
				},
				WorkerSubnet: testSubnetWorkers,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.validate()
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("error %q does not contain %q", err.Error(), c.wantErr)
			}
		})
	}
}

func TestVPCResolvedWorkerSubnet(t *testing.T) {
	// Single subnet → that one. No need to set worker_subnet explicitly.
	cfg := vpcConfig{Subnets: map[string]subnetConfig{testSubnetCache: {IPv4: testCIDRCache}}}
	if got := cfg.resolvedWorkerSubnet(); got != testSubnetCache {
		t.Errorf("single subnet: got %q, want %q", got, testSubnetCache)
	}

	// Multiple subnets, no override → alphabetical first. "a" beats "z".
	cfg = vpcConfig{Subnets: map[string]subnetConfig{
		"z": {IPv4: testCIDRWorkers},
		"a": {IPv4: testCIDRCache},
	}}
	if got := cfg.resolvedWorkerSubnet(); got != "a" {
		t.Errorf("alphabetical-first: got %q, want %q", got, "a")
	}

	// Override wins regardless of order.
	cfg.WorkerSubnet = "z"
	if got := cfg.resolvedWorkerSubnet(); got != "z" {
		t.Errorf("override: got %q, want %q", got, "z")
	}
}

func TestVPCLabel(t *testing.T) {
	cases := []struct {
		name     string
		inName   string
		inTag    string
		mustHave string
		minLen   int
		maxLen   int
	}{
		{
			name: "tag default with prefix", inTag: "fj-bellows",
			mustHave: "fj-bellows-fj-bellows", minLen: 1, maxLen: 64,
		},
		{
			name: "explicit name without prefix", inName: "my-vpc", inTag: "ignored",
			mustHave: "my-vpc", minLen: 1, maxLen: 64,
		},
		{
			name: "underscore in tag sanitized to dash", inTag: "deploy_one",
			mustHave: "fj-bellows-deploy-one", minLen: 1, maxLen: 64,
		},
		{
			name: "dot in tag sanitized to dash (stricter than firewall)", inTag: "v1.0",
			mustHave: "fj-bellows-v1-0", minLen: 1, maxLen: 64,
		},
		{
			name: "long tag truncated with hash", inTag: strings.Repeat("a", 80),
			mustHave: "fj-bellows-", minLen: 1, maxLen: 64,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := vpcLabel(c.inName, c.inTag)
			if len(got) < c.minLen || len(got) > c.maxLen {
				t.Errorf("len(%q) = %d, want between %d and %d", got, len(got), c.minLen, c.maxLen)
			}
			if !strings.Contains(got, c.mustHave) {
				t.Errorf("got %q, want substring %q", got, c.mustHave)
			}
			if strings.ContainsAny(got, "_.") {
				t.Errorf("VPC label %q contains forbidden chars (underscore/dot)", got)
			}
		})
	}
}

func TestVPCLabelLongTagsDontCollide(t *testing.T) {
	a := vpcLabel("", strings.Repeat("a", 80))
	b := vpcLabel("", strings.Repeat("b", 80))
	if a == b {
		t.Errorf("two distinct 80-char tags collided: both → %q", a)
	}
}

func TestSubnetLabelNamespacedByTag(t *testing.T) {
	a := subnetLabel("deploy-a", testSubnetCache)
	b := subnetLabel("deploy-b", testSubnetCache)
	if a == b {
		t.Errorf("same subnet name across deployments collided: both → %q", a)
	}
	if !strings.Contains(a, testSubnetCache) {
		t.Errorf("subnet label %q dropped the subnet name", a)
	}
}

func TestEnsureAtConfigureCreatesFresh(t *testing.T) {
	ctx := context.Background()
	fake := newFakeVPCClient()
	cfg := vpcConfig{Subnets: map[string]subnetConfig{
		testSubnetCache: {IPv4: testCIDRCache},
	}}
	m := newManagedVPC(cfg, "test-tag", "us-ord", fake, slog.Default())

	if err := m.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	if m.id == 0 {
		t.Fatalf("expected VPC ID populated, got 0")
	}
	if got := m.workerSubnetID(); got == 0 {
		t.Errorf("expected worker subnet ID populated, got 0")
	}
	if fake.createCalls != 1 {
		t.Errorf("expected 1 CreateVPC call, got %d", fake.createCalls)
	}
	// CreateVPC carried the subnet inline; no separate CreateVPCSubnet.
	if fake.createSubnetCalls != 0 {
		t.Errorf("expected 0 CreateVPCSubnet calls (inline create), got %d", fake.createSubnetCalls)
	}
}

func TestEnsureAtConfigureAdoptsExistingVPCAndCreatesMissingSubnet(t *testing.T) {
	ctx := context.Background()
	fake := newFakeVPCClient()
	// Seed an existing VPC with only one of the two configured subnets.
	fake.nextID = 100
	fake.vpcs[101] = &linodego.VPC{
		ID:     101,
		Label:  vpcLabel("", "test-tag"),
		Region: "us-ord",
		Subnets: []linodego.VPCSubnet{
			{ID: 200, Label: subnetLabel("test-tag", testSubnetCache), IPv4: testCIDRCache},
		},
	}
	cfg := vpcConfig{Subnets: map[string]subnetConfig{
		testSubnetCache:   {IPv4: testCIDRCache},
		testSubnetWorkers: {IPv4: testCIDRWorkers},
	}}
	m := newManagedVPC(cfg, "test-tag", "us-ord", fake, slog.Default())

	if err := m.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	if m.id != 101 {
		t.Errorf("expected to adopt existing VPC 101, got %d", m.id)
	}
	if m.subnetIDs[testSubnetCache] != 200 {
		t.Errorf("expected to adopt existing subnet 200 for cache, got %d", m.subnetIDs[testSubnetCache])
	}
	if m.subnetIDs[testSubnetWorkers] == 0 {
		t.Errorf("expected workers subnet created and recorded")
	}
	if fake.createCalls != 0 {
		t.Errorf("expected 0 CreateVPC calls (adopt-existing path), got %d", fake.createCalls)
	}
	if fake.createSubnetCalls != 1 {
		t.Errorf("expected 1 CreateVPCSubnet (for missing 'workers'), got %d", fake.createSubnetCalls)
	}
}

func TestEnsureAtConfigureIsIdempotent(t *testing.T) {
	ctx := context.Background()
	fake := newFakeVPCClient()
	cfg := vpcConfig{Subnets: map[string]subnetConfig{
		testSubnetCache: {IPv4: testCIDRCache},
	}}
	m1 := newManagedVPC(cfg, "test-tag", "us-ord", fake, slog.Default())
	if err := m1.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("first ensureAtConfigure: %v", err)
	}
	createdID := m1.id

	// New manager (e.g. process restart) — should adopt, not duplicate.
	m2 := newManagedVPC(cfg, "test-tag", "us-ord", fake, slog.Default())
	if err := m2.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("second ensureAtConfigure: %v", err)
	}
	if m2.id != createdID {
		t.Errorf("expected adopt-existing on restart (id %d), got %d", createdID, m2.id)
	}
	if fake.createCalls != 1 {
		t.Errorf("expected exactly 1 CreateVPC across two Configures, got %d", fake.createCalls)
	}
}

func TestMaybeCleanupVPCSkipsWhenLinodesAttached(t *testing.T) {
	ctx := context.Background()
	fake := newFakeVPCClient()
	cfg := vpcConfig{Subnets: map[string]subnetConfig{
		testSubnetCache: {IPv4: testCIDRCache},
	}}
	m := newManagedVPC(cfg, "test-tag", "us-ord", fake, slog.Default())
	if err := m.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	// Pretend a worker is still on the subnet.
	subnetID := m.workerSubnetID()
	fake.subnetLinodes[subnetID] = []linodego.VPCSubnetLinode{{ID: 9999}}

	m.maybeCleanupVPC(ctx)

	if fake.deleteCalls != 0 || fake.deleteSubnetCalls != 0 {
		t.Errorf("expected no deletes while linodes attached, got vpc=%d subnet=%d",
			fake.deleteCalls, fake.deleteSubnetCalls)
	}
	if _, ok := fake.vpcs[m.id]; !ok {
		t.Errorf("VPC was unexpectedly deleted while linodes attached")
	}
}

func TestMaybeCleanupVPCDeletesWhenIdle(t *testing.T) {
	ctx := context.Background()
	fake := newFakeVPCClient()
	cfg := vpcConfig{Subnets: map[string]subnetConfig{
		testSubnetCache:   {IPv4: testCIDRCache},
		testSubnetWorkers: {IPv4: testCIDRWorkers},
	}}
	m := newManagedVPC(cfg, "test-tag", "us-ord", fake, slog.Default())
	if err := m.ensureAtConfigure(ctx); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	createdVPCID := m.id

	m.maybeCleanupVPC(ctx)

	if fake.deleteSubnetCalls != 2 {
		t.Errorf("expected 2 DeleteVPCSubnet calls, got %d", fake.deleteSubnetCalls)
	}
	if fake.deleteCalls != 1 {
		t.Errorf("expected 1 DeleteVPC call, got %d", fake.deleteCalls)
	}
	if _, ok := fake.vpcs[createdVPCID]; ok {
		t.Errorf("VPC %d still present after cleanup", createdVPCID)
	}
	if m.id != 0 {
		t.Errorf("expected m.id reset to 0 after cleanup, got %d", m.id)
	}
}

func TestMaybeCleanupVPCNoOpWhenAbsent(t *testing.T) {
	ctx := context.Background()
	fake := newFakeVPCClient()
	cfg := vpcConfig{Subnets: map[string]subnetConfig{
		testSubnetCache: {IPv4: testCIDRCache},
	}}
	m := newManagedVPC(cfg, "test-tag", "us-ord", fake, slog.Default())
	// Never ensured — no VPC exists.
	m.maybeCleanupVPC(ctx)

	if fake.deleteCalls != 0 || fake.deleteSubnetCalls != 0 {
		t.Errorf("expected no deletes when no VPC exists, got vpc=%d subnet=%d",
			fake.deleteCalls, fake.deleteSubnetCalls)
	}
}

// linodego.Client must satisfy our interface in vpc.go; this test catches
// drift if linodego changes a signature underneath us.
func TestVPCClientInterfaceCompiles(_ *testing.T) {
	var _ vpcClient = (*linodego.Client)(nil)
}
