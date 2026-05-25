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

// fakePlacementGroupClient is a hand-rolled placementGroupClient (per repo
// conventions — no codegen). Stores groups in a map keyed by ID; tests can
// pre-seed `groups` to exercise the find-or-create path, and inject
// members to exercise maybeCleanupPlacementGroup.
type fakePlacementGroupClient struct {
	mu     sync.Mutex
	groups map[int]*linodego.PlacementGroup
	nextID int

	listCalls   int
	getCalls    int
	createCalls int
	deleteCalls int
}

func newFakePlacementGroupClient() *fakePlacementGroupClient {
	return &fakePlacementGroupClient{
		groups: map[int]*linodego.PlacementGroup{},
	}
}

func (f *fakePlacementGroupClient) ListPlacementGroups(_ context.Context, _ *linodego.ListOptions) ([]linodego.PlacementGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	out := make([]linodego.PlacementGroup, 0, len(f.groups))
	for _, v := range f.groups {
		out = append(out, *v)
	}
	return out, nil
}

func (f *fakePlacementGroupClient) GetPlacementGroup(_ context.Context, id int) (*linodego.PlacementGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	pg, ok := f.groups[id]
	if !ok {
		return nil, errors.New("not found")
	}
	cp := *pg
	return &cp, nil
}

func (f *fakePlacementGroupClient) CreatePlacementGroup(_ context.Context, opts linodego.PlacementGroupCreateOptions) (*linodego.PlacementGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.nextID++
	pg := &linodego.PlacementGroup{
		ID:                   f.nextID,
		Label:                opts.Label,
		Region:               opts.Region,
		PlacementGroupType:   opts.PlacementGroupType,
		PlacementGroupPolicy: opts.PlacementGroupPolicy,
	}
	f.groups[pg.ID] = pg
	return pg, nil
}

func (f *fakePlacementGroupClient) DeletePlacementGroup(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	delete(f.groups, id)
	return nil
}

func TestPlacementGroupLabel(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		minLen   int
		maxLen   int
		mustHave string
	}{
		{name: "short tag fits", in: testLabelPrefix, minLen: 1, maxLen: 64, mustHave: "fj-bellows-fj-bellows"},
		{name: "valid charset preserved", in: "deploy.one_two-3", minLen: 1, maxLen: 64, mustHave: "fj-bellows-deploy.one_two-3"},
		{name: "invalid chars sanitized", in: "weird/chars!", minLen: 1, maxLen: 64, mustHave: "fj-bellows-weird-chars-"},
		// 64-char cap is wider than firewall's 32; only TRULY long tags
		// need truncation. Use 80 to exceed.
		{name: "long tag truncated with hash", in: strings.Repeat("a", 80), minLen: 1, maxLen: 64, mustHave: "fj-bellows-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := placementGroupLabel(c.in)
			if len(got) < c.minLen || len(got) > c.maxLen {
				t.Errorf("len(%q) = %d, want between %d and %d", got, len(got), c.minLen, c.maxLen)
			}
			if !strings.Contains(got, c.mustHave) {
				t.Errorf("got %q, want substring %q", got, c.mustHave)
			}
		})
	}
}

func TestPlacementGroupLabelDifferentLongTagsDontCollide(t *testing.T) {
	a := placementGroupLabel(strings.Repeat("a", 80))
	b := placementGroupLabel(strings.Repeat("b", 80))
	if a == b {
		t.Errorf("two distinct 80-char tags collided: both → %q", a)
	}
}

func TestPlacementGroupConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     placementGroupConfig
		wantErr bool
	}{
		{name: "empty default", cfg: placementGroupConfig{}, wantErr: false},
		{name: "flexible", cfg: placementGroupConfig{Enforcement: "flexible"}, wantErr: false},
		{name: string(linodego.PlacementGroupPolicyStrict), cfg: placementGroupConfig{Enforcement: string(linodego.PlacementGroupPolicyStrict)}, wantErr: false},
		{name: "typo", cfg: placementGroupConfig{Enforcement: "loose"}, wantErr: true},
		{name: "case sensitive", cfg: placementGroupConfig{Enforcement: "Strict"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.validate()
			if (err != nil) != c.wantErr {
				t.Errorf("validate() err=%v, wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestPlacementGroupConfigResolvedPolicyDefaultsToFlexible(t *testing.T) {
	if got := (placementGroupConfig{}).resolvedPolicy(); got != linodego.PlacementGroupPolicyFlexible {
		t.Errorf("empty Enforcement should resolve to flexible, got %q", got)
	}
	if got := (placementGroupConfig{Enforcement: string(linodego.PlacementGroupPolicyStrict)}).resolvedPolicy(); got != linodego.PlacementGroupPolicyStrict {
		t.Errorf("Enforcement=strict should resolve to strict, got %q", got)
	}
}

func TestEnsurePlacementGroupCreatesWhenAbsent(t *testing.T) {
	fake := newFakePlacementGroupClient()
	m := newManagedPlacementGroup(placementGroupConfig{}, testTag, "us-ord", fake, slog.Default())
	if err := m.ensureAtConfigure(context.Background()); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	if fake.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", fake.createCalls)
	}
	if m.id == 0 {
		t.Fatal("expected non-zero id")
	}
	pg := fake.groups[m.id]
	if pg.Label != placementGroupLabel(testTag) {
		t.Errorf("label = %q, want %q", pg.Label, placementGroupLabel(testTag))
	}
	if pg.Region != "us-ord" {
		t.Errorf("region = %q, want us-ord", pg.Region)
	}
	if pg.PlacementGroupType != linodego.PlacementGroupTypeAntiAffinityLocal {
		t.Errorf("type = %q, want anti_affinity:local", pg.PlacementGroupType)
	}
	if pg.PlacementGroupPolicy != linodego.PlacementGroupPolicyFlexible {
		t.Errorf("policy = %q, want flexible (default)", pg.PlacementGroupPolicy)
	}
}

func TestEnsurePlacementGroupReusesByLabel(t *testing.T) {
	fake := newFakePlacementGroupClient()
	m := newManagedPlacementGroup(placementGroupConfig{}, testTag, "us-ord", fake, slog.Default())
	if err := m.ensureAtConfigure(context.Background()); err != nil {
		t.Fatalf("ensureAtConfigure (first): %v", err)
	}
	firstID := m.id

	// Second managedPlacementGroup with the same tag should reuse, not
	// recreate. Important for daemon restart: the existing group is
	// adopted rather than a duplicate created (Linode would actually
	// accept a duplicate label, but we'd then have two groups owned by
	// the same deployment label, which is wrong).
	m2 := newManagedPlacementGroup(placementGroupConfig{}, testTag, "us-ord", fake, slog.Default())
	if err := m2.ensureAtConfigure(context.Background()); err != nil {
		t.Fatalf("ensureAtConfigure (second): %v", err)
	}
	if m2.id != firstID {
		t.Errorf("second ensureAtConfigure produced ID %d, want reuse of %d", m2.id, firstID)
	}
	if fake.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (no duplicate create)", fake.createCalls)
	}
}

func TestEnsurePlacementGroupTagIsolation(t *testing.T) {
	// Two distinct deployments on the same Linode account get distinct
	// placement groups.
	fake := newFakePlacementGroupClient()
	a := newManagedPlacementGroup(placementGroupConfig{}, "deploy-a", "us-ord", fake, slog.Default())
	b := newManagedPlacementGroup(placementGroupConfig{}, "deploy-b", "us-ord", fake, slog.Default())
	if err := a.ensureAtConfigure(context.Background()); err != nil {
		t.Fatalf("ensureAtConfigure (a): %v", err)
	}
	if err := b.ensureAtConfigure(context.Background()); err != nil {
		t.Fatalf("ensureAtConfigure (b): %v", err)
	}
	if a.id == b.id {
		t.Errorf("two distinct tags collided on one PG (%d)", a.id)
	}
	if fake.createCalls != 2 {
		t.Errorf("createCalls = %d, want 2", fake.createCalls)
	}
}

func TestEnsurePlacementGroupStrictPolicyPassedThrough(t *testing.T) {
	fake := newFakePlacementGroupClient()
	m := newManagedPlacementGroup(placementGroupConfig{Enforcement: string(linodego.PlacementGroupPolicyStrict)}, testTag, "us-ord", fake, slog.Default())
	if err := m.ensureAtConfigure(context.Background()); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	pg := fake.groups[m.id]
	if pg.PlacementGroupPolicy != linodego.PlacementGroupPolicyStrict {
		t.Errorf("policy = %q, want strict", pg.PlacementGroupPolicy)
	}
}

func TestMaybeCleanupPlacementGroupDeletesWhenEmpty(t *testing.T) {
	fake := newFakePlacementGroupClient()
	m := newManagedPlacementGroup(placementGroupConfig{}, testTag, "us-ord", fake, slog.Default())
	if err := m.ensureAtConfigure(context.Background()); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	// no members → cleanup deletes.
	m.maybeCleanupPlacementGroup(context.Background())
	if fake.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", fake.deleteCalls)
	}
	if _, exists := fake.groups[m.id]; exists {
		t.Error("placement group still in fake store after cleanup")
	}
}

func TestMaybeCleanupPlacementGroupSkipsWhenMembersAttached(t *testing.T) {
	fake := newFakePlacementGroupClient()
	m := newManagedPlacementGroup(placementGroupConfig{}, testTag, "us-ord", fake, slog.Default())
	if err := m.ensureAtConfigure(context.Background()); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	// Inject a member to simulate a Linode that hasn't been destroyed yet.
	fake.groups[m.id].Members = []linodego.PlacementGroupMember{{LinodeID: 999, IsCompliant: true}}
	m.maybeCleanupPlacementGroup(context.Background())
	if fake.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0 (members still attached)", fake.deleteCalls)
	}
}

func TestMaybeCleanupPlacementGroupNoOpWhenNotFound(t *testing.T) {
	fake := newFakePlacementGroupClient()
	m := newManagedPlacementGroup(placementGroupConfig{}, testTag, "us-ord", fake, slog.Default())
	// no ensureAtConfigure call → no group exists
	m.maybeCleanupPlacementGroup(context.Background())
	if fake.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0", fake.deleteCalls)
	}
}

// TestPlacementGroupEnsureRecreatesAfterReap is the FJB-10 regression:
// after the reaper has cleared the cached ID (deleted the group on
// last-Destroy), the next Provision must NOT send PlacementGroup.ID=0
// to Linode. ensure() restores the group via ensureAtConfigure.
func TestPlacementGroupEnsureRecreatesAfterReap(t *testing.T) {
	fake := newFakePlacementGroupClient()
	m := newManagedPlacementGroup(placementGroupConfig{}, testTag, "us-ord", fake, slog.Default())
	if err := m.ensureAtConfigure(context.Background()); err != nil {
		t.Fatalf("initial ensureAtConfigure: %v", err)
	}
	firstID := m.id
	if firstID == 0 {
		t.Fatal("initial ensure failed to set m.id")
	}
	// Simulate the reaper having torn down the group and cleared the ID.
	m.maybeCleanupPlacementGroup(context.Background())
	if m.id != 0 {
		t.Fatalf("post-reap m.id = %d, want 0 (precondition broken)", m.id)
	}

	beforeCreate := fake.createCalls
	if err := m.ensure(context.Background()); err != nil {
		t.Fatalf("ensure() after reap: %v", err)
	}
	if m.id == 0 {
		t.Error("ensure() left m.id = 0; would re-send Placement Group ID: 0 not found to Linode (FJB-10)")
	}
	if fake.createCalls != beforeCreate+1 {
		t.Errorf("createCalls = %d, want %d (ensure() should have re-created the group)",
			fake.createCalls, beforeCreate+1)
	}
}

// TestPlacementGroupEnsureNoOpWhenIDStillValid asserts the common-
// case is genuinely a no-op — no API calls on steady-state Provision,
// otherwise we'd flood the Linode API with redundant List/Get calls.
func TestPlacementGroupEnsureNoOpWhenIDStillValid(t *testing.T) {
	fake := newFakePlacementGroupClient()
	m := newManagedPlacementGroup(placementGroupConfig{}, testTag, "us-ord", fake, slog.Default())
	if err := m.ensureAtConfigure(context.Background()); err != nil {
		t.Fatalf("ensureAtConfigure: %v", err)
	}
	beforeList := fake.listCalls
	beforeCreate := fake.createCalls
	if err := m.ensure(context.Background()); err != nil {
		t.Fatalf("ensure(): %v", err)
	}
	if fake.listCalls != beforeList || fake.createCalls != beforeCreate {
		t.Errorf("ensure() with valid m.id should be a no-op; got listCalls %d→%d, createCalls %d→%d",
			beforeList, fake.listCalls, beforeCreate, fake.createCalls)
	}
}

// fakePlacementGroupClient satisfies the interface.
var _ placementGroupClient = (*fakePlacementGroupClient)(nil)
