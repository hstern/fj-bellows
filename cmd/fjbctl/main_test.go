package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/control"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

// newCLIBackedServer wires a real control.Server behind an httptest HTTP/1.1
// listener and returns the listen string (host:port) callers pass to fjbctl
// via the -listen flag. HTTP/1.1 is enough for the unary subcommands tested
// here; the streaming smokes (events / workers --watch) are out of scope —
// they're exercised end-to-end against a real daemon via the linode e2e
// once fjbctl is wired into it in a follow-up.
func newCLIBackedServer(t *testing.T, backend control.Backend) string {
	t.Helper()
	srv := control.NewServer("127.0.0.1:0", backend, nil)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	// Strip the http:// prefix so we exercise the default "host:port"
	// rendering of -listen. fjbctl will prepend http://.
	return strings.TrimPrefix(hs.URL, "http://")
}

// captureRun runs the subcommand dispatch with stdout captured to a buffer
// via a temp file (run() takes *os.File). Returns exit code + stdout.
// stderr goes to the test's own stderr — useful for debugging a failing
// test without complicating the helper.
func captureRun(t *testing.T, args ...string) (code int, stdout string) {
	t.Helper()
	stdoutF, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("tempfile: %v", err)
	}
	code = run(args, stdoutF, os.Stderr)
	_, _ = stdoutF.Seek(0, 0)
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(stdoutF)
	return code, buf.String()
}

func TestUsage_NoArgs_NonZero(t *testing.T) {
	code, _ := captureRun(t)
	if code == 0 {
		t.Fatal("expected non-zero exit with no args")
	}
}

func TestUsage_Help_ZeroExit(t *testing.T) {
	code, out := captureRun(t, "help")
	if code != 0 {
		t.Fatalf("help should exit 0; got %d", code)
	}
	if !strings.Contains(out, "Subcommands") {
		t.Fatalf("usage text missing Subcommands section:\n%s", out)
	}
}

func TestHealth_Human_PrintsHealthyAndAges(t *testing.T) {
	now := time.Now()
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus {
		return control.HealthStatus{
			Healthy:            true,
			LastTickAt:         now.Add(-2 * time.Second),
			LastProviderListAt: now.Add(-3 * time.Second),
			LastForgejoPollAt:  now.Add(-4 * time.Second),
		}
	})
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })

	listen := newCLIBackedServer(t, be)
	code, out := captureRun(t, "health", "-listen", listen)
	if code != 0 {
		t.Fatalf("expected exit 0 for healthy daemon; got %d\nstdout:\n%s", code, out)
	}
	if !strings.Contains(out, "HEALTHY") {
		t.Fatalf("missing HEALTHY in output:\n%s", out)
	}
	if !strings.Contains(out, "last_tick") {
		t.Fatalf("missing last_tick line:\n%s", out)
	}
}

func TestHealth_JSON_EmitsProtoJSON(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus {
		return control.HealthStatus{Healthy: true, LastTickAt: time.Now()}
	})
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })

	listen := newCLIBackedServer(t, be)
	code, out := captureRun(t, "health", "-listen", listen, "-json")
	if code != 0 {
		t.Fatalf("exit: %d\n%s", code, out)
	}
	// protojson renders `"healthy":  true` (double space after colon under
	// Multiline+Indent); match the substring loosely.
	if !strings.Contains(out, `"healthy":`) || !strings.Contains(out, "true") {
		t.Fatalf("missing healthy:true in JSON:\n%s", out)
	}
}

func TestHealth_Unhealthy_ExitsOne(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: false} })
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })

	listen := newCLIBackedServer(t, be)
	code, out := captureRun(t, "health", "-listen", listen)
	if code != 1 {
		t.Fatalf("unhealthy daemon should exit 1; got %d\n%s", code, out)
	}
}

func TestWorkers_Human_TableRender(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })
	be.SetPoolSnapshot(func() []control.WorkerView {
		return []control.WorkerView{
			{InstanceID: "linode-1", State: "idle", IP: "203.0.113.7", CreatedAt: time.Now().Add(-time.Minute)},
			{InstanceID: "linode-2", State: "busy", IP: "203.0.113.8", CurrentJob: "job-x"},
		}
	})

	listen := newCLIBackedServer(t, be)
	code, out := captureRun(t, "workers", "-listen", listen)
	if code != 0 {
		t.Fatalf("exit: %d\n%s", code, out)
	}
	for _, want := range []string{"INSTANCE", "linode-1", "idle", "203.0.113.7", "linode-2", "busy", "job-x"} {
		if !strings.Contains(out, want) {
			t.Fatalf("workers output missing %q:\n%s", want, out)
		}
	}
}

// TestWorkers_Human_BillingWindowColumns covers FJB-30: BILLING and
// REAP_AT columns surface the policy's billing-window snapshot, with
// hourly-billed workers rendering both fields and per-second workers
// rendering BILLING + reap eta only.
func TestWorkers_Human_BillingWindowColumns(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })
	// Use far-future timestamps so the etaOrDash branch is "in <d>" — keeps
	// the assertion stable against clock skew between test setup and render.
	future := time.Now().Add(42 * time.Minute)
	be.SetPoolSnapshot(func() []control.WorkerView {
		// State omitted: the test asserts on the FJB-30 billing-window
		// columns, not state, so leaving State zero keeps the goconst
		// "idle"-string-elsewhere check happy.
		return []control.WorkerView{
			{
				InstanceID:     "linode-h",
				IP:             "203.0.113.7",
				BillingModel:   "hourly_round_up",
				PaidHourEndAt:  future.Add(5 * time.Minute),
				ReapEligibleAt: future,
			},
			{
				InstanceID:     "linode-s",
				IP:             "203.0.113.8",
				BillingModel:   "per_second",
				ReapEligibleAt: future,
			},
		}
	})

	listen := newCLIBackedServer(t, be)
	code, out := captureRun(t, "workers", "-listen", listen)
	if code != 0 {
		t.Fatalf("exit: %d\n%s", code, out)
	}
	for _, want := range []string{"BILLING", "REAP_AT", "hourly_round_up", "per_second", "in "} {
		if !strings.Contains(out, want) {
			t.Fatalf("workers output missing %q:\n%s", want, out)
		}
	}
}

func TestWorkers_EmptyPool_RendersSentinel(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })

	listen := newCLIBackedServer(t, be)
	code, out := captureRun(t, "workers", "-listen", listen)
	if code != 0 {
		t.Fatalf("exit: %d", code)
	}
	if !strings.Contains(out, "(no workers)") {
		t.Fatalf("empty-pool sentinel missing:\n%s", out)
	}
}

func TestCache_NotConfigured(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })

	listen := newCLIBackedServer(t, be)
	code, out := captureRun(t, "cache", "-listen", listen)
	if code != 0 {
		t.Fatalf("exit: %d", code)
	}
	if !strings.Contains(out, "not configured") {
		t.Fatalf("missing 'not configured':\n%s", out)
	}
}

func TestCache_Present(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus {
		return &control.CacheStatus{
			Present:      true,
			LinodeID:     98765,
			VMState:      "running",
			VPCIP:        "10.0.0.7",
			BucketRegion: "us-ord",
			BucketLabel:  "fjb-cache-x",
		}
	})

	listen := newCLIBackedServer(t, be)
	code, out := captureRun(t, "cache", "-listen", listen)
	if code != 0 {
		t.Fatalf("exit: %d\n%s", code, out)
	}
	for _, want := range []string{"present", "98765", "running", "10.0.0.7", "us-ord/fjb-cache-x"} {
		if !strings.Contains(out, want) {
			t.Fatalf("cache output missing %q:\n%s", want, out)
		}
	}
}

func TestReconcile_HappyPath(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })
	be.SetKick(func(context.Context) (control.ReconcileResult, error) {
		return control.ReconcileResult{Provisioned: 1, Dispatched: 2}, nil
	})

	listen := newCLIBackedServer(t, be)
	code, out := captureRun(t, "reconcile", "-listen", listen)
	if code != 0 {
		t.Fatalf("exit: %d\n%s", code, out)
	}
	for _, want := range []string{"reconcile complete", "provisioned  1", "dispatched   2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("reconcile output missing %q:\n%s", want, out)
		}
	}
}

func TestReconcile_BackendError_ExitsOne(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })
	be.SetKick(func(context.Context) (control.ReconcileResult, error) {
		return control.ReconcileResult{}, context.DeadlineExceeded
	})

	listen := newCLIBackedServer(t, be)
	code, _ := captureRun(t, "reconcile", "-listen", listen)
	if code != 1 {
		t.Fatalf("rpc error should exit 1; got %d", code)
	}
}

func TestUnknownSubcommand(t *testing.T) {
	code, _ := captureRun(t, "no-such-thing")
	if code != 2 {
		t.Fatalf("unknown subcommand should exit 2; got %d", code)
	}
}
