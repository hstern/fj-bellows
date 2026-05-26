package linode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linode/linodego"
)

// stubAccountServer returns an httptest server that responds to
// GET /account with status. body is the JSON returned on a 200; ignored
// otherwise. Tests use this so the linodego client has a real (non-nil)
// HTTP backend without us depending on a live Linode endpoint.
func stubAccountServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withStubAccountClient wires l.client at a stub HTTP server speaking
// the Linode v4 account shape, so info.go's GetAccount call doesn't
// reach the real Linode API in tests.
func withStubAccountClient(t *testing.T, l *Linode, status int, body string) {
	t.Helper()
	srv := stubAccountServer(t, status, body)
	c := linodego.NewClient(srv.Client())
	c.SetBaseURL(srv.URL)
	c.SetToken("test-token") // any non-empty value
	l.client = c
}

// TestInfo_BasicShape covers the no-managed-resources path: every key
// the operator expects is present, the cfg values pass through, the
// counters default to "0", and the account_balance_usd key falls back
// to "" when GetAccount fails (the zero-value client has no token /
// can't dial out — error path).
func TestInfo_BasicShape(t *testing.T) {
	l := &Linode{
		cfg: config{
			Region: testBucketRegion,
			Type:   defaultCacheType,
			Image:  defaultCacheImage,
		},
	}
	withStubAccountClient(t, l, http.StatusUnauthorized, `{"errors":[{"reason":"no scope"}]}`)
	info := l.Info(context.Background())

	wantKeys := []string{
		"region", "type", "image",
		"firewall_id", "placement_group_id", "vpc_id", "cache_linode_id",
		"workers_in_flight", "capacity_full_count_24h", "account_balance_usd",
	}
	for _, k := range wantKeys {
		if _, ok := info[k]; !ok {
			t.Errorf("Info() missing key %q", k)
		}
	}
	if got := info["region"]; got != testBucketRegion {
		t.Errorf("region: want %s got %q", testBucketRegion, got)
	}
	if got := info["type"]; got != defaultCacheType {
		t.Errorf("type: want %s got %q", defaultCacheType, got)
	}
	if got := info["image"]; got != defaultCacheImage {
		t.Errorf("image: want %s got %q", defaultCacheImage, got)
	}
	// Default zero values stringify to "0".
	for _, k := range []string{"firewall_id", "placement_group_id", "vpc_id", "cache_linode_id", "workers_in_flight", "capacity_full_count_24h"} {
		if got := info[k]; got != "0" {
			t.Errorf("%s: want 0 got %q", k, got)
		}
	}
	// account_balance_usd is "" when GetAccount can't be reached. The
	// zero-value linodego.Client + a context.Background ctx doesn't
	// reach Linode; we don't depend on the exact failure mode, only
	// that the key falls back to empty per the documented contract.
	if got := info["account_balance_usd"]; got != "" {
		t.Errorf("account_balance_usd should be empty on unreachable account API, got %q", got)
	}
}

// TestInfo_AttachByIDFallsThrough covers the firewall_id / placement_group_id
// modes: when the operator attaches by ID instead of asking fjb to manage
// the resource, Info() reports the configured ID rather than 0.
func TestInfo_AttachByIDFallsThrough(t *testing.T) {
	l := &Linode{
		cfg: config{
			Region:           testBucketRegion,
			Type:             defaultCacheType,
			Image:            defaultCacheImage,
			FirewallID:       111,
			PlacementGroupID: 222,
		},
	}
	withStubAccountClient(t, l, http.StatusUnauthorized, "")
	info := l.Info(context.Background())
	if got := info["firewall_id"]; got != "111" {
		t.Errorf("firewall_id: want 111 got %q", got)
	}
	if got := info["placement_group_id"]; got != "222" {
		t.Errorf("placement_group_id: want 222 got %q", got)
	}
}

// TestCapacityFullRing_RollingWindow covers the rolling-window semantic:
// notes within the 24h window count, anything older is pruned, and the
// ring tops out at capacityFullRingMax entries (drop-oldest on push).
func TestCapacityFullRing_RollingWindow(t *testing.T) {
	var r capacityFullRing
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)

	// Three recent notes: all counted.
	r.note(now.Add(-1 * time.Minute))
	r.note(now.Add(-2 * time.Hour))
	r.note(now.Add(-23 * time.Hour))
	if got := r.count(now); got != 3 {
		t.Errorf("count: want 3 got %d", got)
	}

	// One stale note outside the window: dropped on next count.
	r.note(now.Add(-25 * time.Hour))
	if got := r.count(now); got != 3 {
		t.Errorf("count after stale note: want 3 got %d", got)
	}
}

// TestCapacityFullRing_BoundedAtMax covers the bounded-memory guarantee:
// pushing more than capacityFullRingMax notes within the window doesn't
// grow the ring past the bound; the oldest entry is evicted on each push.
func TestCapacityFullRing_BoundedAtMax(t *testing.T) {
	var r capacityFullRing
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	// Note 2*cap inside the window. After the first cap, each push
	// drops the oldest, so the surviving set is the most-recent cap.
	for i := range capacityFullRingMax * 2 {
		// Spread across the past hour so all stay inside the 24h window.
		r.note(now.Add(-time.Duration(i) * time.Second).Add(-time.Hour))
	}
	if got := r.count(now); got != capacityFullRingMax {
		t.Errorf("ring should be bounded at %d, got %d", capacityFullRingMax, got)
	}
}

// TestInfo_AccountBalanceFormatted covers the happy path of the
// /account call: a 200 with a positive balance is formatted to two
// decimals; a 200 with a negative balance (the operator owes Linode)
// is preserved sign-and-all so the operator can spot the dunning case.
func TestInfo_AccountBalanceFormatted(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantUSD string
	}{
		{name: "positive", body: `{"balance":12.34}`, wantUSD: "12.34"},
		{name: "negative", body: `{"balance":-7.5}`, wantUSD: "-7.50"},
		{name: "zero", body: `{"balance":0}`, wantUSD: "0.00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := &Linode{cfg: config{Region: "us-ord", Type: "g6-nanode-1", Image: "linode/debian13"}}
			withStubAccountClient(t, l, http.StatusOK, c.body)
			info := l.Info(context.Background())
			if got := info["account_balance_usd"]; got != c.wantUSD {
				t.Errorf("account_balance_usd: want %q got %q", c.wantUSD, got)
			}
		})
	}
}

// TestInfo_WorkersInFlightReflectsAtomic covers the workers_in_flight
// key: incrementing the atomic counter is reflected in the next Info()
// call without any extra plumbing.
func TestInfo_WorkersInFlightReflectsAtomic(t *testing.T) {
	l := &Linode{
		cfg: config{Region: testBucketRegion, Type: defaultCacheType, Image: defaultCacheImage},
	}
	withStubAccountClient(t, l, http.StatusUnauthorized, "")
	l.workersInFlight.Add(3)
	info := l.Info(context.Background())
	if got := info["workers_in_flight"]; got != "3" {
		t.Errorf("workers_in_flight: want 3 got %q", got)
	}
	l.workersInFlight.Add(-3)
	info = l.Info(context.Background())
	if got := info["workers_in_flight"]; got != "0" {
		t.Errorf("workers_in_flight after dec: want 0 got %q", got)
	}
}
