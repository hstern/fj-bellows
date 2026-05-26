package control_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/control"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

func TestMetrics_ExposesPulledGauges(t *testing.T) {
	now := time.Now()
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus {
		return control.HealthStatus{Healthy: true, LastTickAt: now.Add(-2 * time.Second)}
	})
	const stateIdle, stateBusy = "idle", "busy"
	be.SetPoolSnapshot(func() []control.WorkerView {
		return []control.WorkerView{
			{InstanceID: "a", State: stateIdle},
			{InstanceID: "b", State: stateBusy},
			{InstanceID: "c", State: stateBusy},
		}
	})
	be.SetCacheStatus(func(context.Context) *control.CacheStatus {
		return &control.CacheStatus{Present: true}
	})

	hs, _ := newTestServer(t, be)
	body := scrapeMetrics(t, hs.Client(), hs.URL)

	mustContain(t, body, `fjb_healthy 1`)
	mustContain(t, body, `fjb_workers_total 3`)
	mustContain(t, body, `fjb_workers{state="idle"} 1`)
	mustContain(t, body, `fjb_workers{state="busy"} 2`)
	mustContain(t, body, `fjb_workers{state="provisioning"} 0`) // pre-seeded
	mustContain(t, body, `fjb_cache_present 1`)
}

func TestMetrics_LastTickAge_NegativeBeforeFirstTick(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus {
		return control.HealthStatus{} // zero LastTickAt
	})
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })

	hs, _ := newTestServer(t, be)
	body := scrapeMetrics(t, hs.Client(), hs.URL)
	mustContain(t, body, `fjb_last_tick_age_seconds -1`)
}

func TestMetrics_EventsTotal_Registered(t *testing.T) {
	// fjb_events_total is a CounterVec wired in newMetrics. The actual
	// counter increment lives in runEventTee, which is started inside
	// Server.Run and isn't exercised by newTestServer here. This test
	// verifies the metric is registered (HELP line present) so a Prom
	// scrape against a freshly-started daemon shows it before any events.
	// The end-to-end "publish-then-scrape-shows-the-bump" path is covered
	// indirectly by the Linode e2e.
	be := &mockctl.Backend{}
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{} })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })

	hs, _ := newTestServer(t, be)
	body := scrapeMetrics(t, hs.Client(), hs.URL)
	mustContain(t, body, "# HELP fjb_events_total")
}

// scrapeMetrics issues a GET /metrics and returns the body as a string.
func scrapeMetrics(t *testing.T, hc *http.Client, baseURL string) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/metrics", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200 got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func mustContain(t *testing.T, body, needle string) {
	t.Helper()
	if !strings.Contains(body, needle) {
		t.Fatalf("metrics body missing %q\nfull body:\n%s", needle, body)
	}
}
