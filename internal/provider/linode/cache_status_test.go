package linode

import (
	"context"
	"log/slog"
	"testing"

	"github.com/linode/linodego"
)

func TestCacheStatus_BeforeEnsure_ReportsAbsent(t *testing.T) {
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	m := newTestManagedCache(t, fc, bucket)

	s := m.Status(context.Background())
	if s.Present {
		t.Fatal("Status before ensureAtConfigure must report Present=false")
	}
	if s.LinodeID != 0 || s.VMState != "" {
		t.Fatalf("absent cache should leave LinodeID/VMState zeroed: %+v", s)
	}
	if s.BucketRegion != testBucketRegion || s.BucketLabel != "fjb-cache-test-tag" {
		t.Fatalf("bucket metadata should be reported even before ensure: %+v", s)
	}
}

func TestCacheStatus_AfterEnsure_QueriesLinodeForVMState(t *testing.T) {
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	m := newTestManagedCache(t, fc, bucket)

	// Simulate "ensureAtConfigure has run": linodeID set, bucket configured,
	// and a corresponding instance present in the fake API.
	m.linodeID = 42
	m.adoptedExisting = false
	m.cacheVPCIP = "10.0.0.7"
	fc.mu.Lock()
	fc.insts[42] = &linodego.Instance{ID: 42, Status: linodego.InstanceRunning}
	fc.mu.Unlock()

	s := m.Status(context.Background())
	if !s.Present {
		t.Fatal("Status after ensure must report Present=true")
	}
	if s.LinodeID != 42 {
		t.Fatalf("LinodeID: want 42 got %d", s.LinodeID)
	}
	if s.VPCIP != "10.0.0.7" {
		t.Fatalf("VPCIP: want 10.0.0.7 got %q", s.VPCIP)
	}
	if s.VMState != string(linodego.InstanceRunning) {
		t.Fatalf("VMState: want %s got %q", linodego.InstanceRunning, s.VMState)
	}
}

func TestCacheStatus_VMLookupFailure_NonFatal(t *testing.T) {
	fc := newFakeCacheClient()
	fb := newFakeBucketClient()
	bucket := newManagedBucket("test-tag", testBucketRegion, "fjb-cache-test-tag", fb, slog.Default())
	m := newTestManagedCache(t, fc, bucket)

	m.linodeID = 99 // not pre-seeded into fc.insts → GetInstance returns an error

	s := m.Status(context.Background())
	if !s.Present {
		t.Fatal("Present should still be true based on local state, even if lookup fails")
	}
	if s.VMState != "" {
		t.Fatalf("VMState should be empty on lookup failure; got %q", s.VMState)
	}
}
