package orchestrator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/forgejo"
	omock "github.com/hstern/fj-bellows/internal/orchestrator/mock"
	"github.com/hstern/fj-bellows/internal/provider"
	pmock "github.com/hstern/fj-bellows/internal/provider/mock"
)

// TestPauseSuppressesAutoTick asserts that once Pause has been called, the
// auto-tick path (ticker.C) no longer drives Reconcile. The kick channel is
// still serviced — that's the operator-explicit-tick contract documented on
// Pause.
func TestPauseSuppressesAutoTick(t *testing.T) {
	var listCalls atomic.Int32
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			listCalls.Add(1)
			return nil, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn:  func(context.Context) ([]forgejo.WaitingJob, error) { return nil, nil },
		ListRunnersFn:  func(context.Context) ([]forgejo.Runner, error) { return nil, nil },
		DeleteRunnerFn: func(context.Context, int64) error { return nil },
		RegisterEphemeralFn: func(context.Context, string, []string) (forgejo.Registration, error) {
			return forgejo.Registration{}, nil
		},
	}
	cfg := baseConfig()
	cfg.PollInterval = 10 * time.Millisecond // fast ticker so we can observe ticks quickly
	o := New(cfg, prov, jobs, &omock.Dispatcher{}, nil)

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = o.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// Wait for the initial reconcile + at least one auto-tick so we know the
	// loop is alive before pausing.
	waitFor(t, "auto-tick fired before pause", func() bool { return listCalls.Load() >= 2 })

	o.Pause(t.Context())
	if !o.IsPaused() {
		t.Fatal("IsPaused should be true after Pause")
	}

	// Read the counter, wait several poll intervals, ensure no further List
	// calls happened.
	before := listCalls.Load()
	time.Sleep(100 * time.Millisecond) // ~10 ticks worth
	after := listCalls.Load()
	if after != before {
		t.Fatalf("list called %d times while paused; want 0 (before=%d after=%d)", after-before, before, after)
	}

	// Explicit kick still works while paused.
	if _, err := o.Kick(t.Context()); err != nil {
		t.Fatalf("Kick while paused: %v", err)
	}
	if listCalls.Load() != before+1 {
		t.Fatalf("Kick should have driven one reconcile while paused; before=%d after=%d", before, listCalls.Load())
	}

	o.Resume(t.Context())
	if o.IsPaused() {
		t.Fatal("IsPaused should be false after Resume")
	}
	waitFor(t, "auto-tick resumed", func() bool { return listCalls.Load() > before+1 })
}

// TestPauseIsIdempotent asserts that Pause()-then-Pause() is a no-op and
// Resume()-then-Resume() likewise. The audit log line only fires on a real
// transition; idempotent calls stay silent (verified through event count).
func TestPauseIsIdempotent(t *testing.T) {
	o := New(baseConfig(), nil, nil, nil, nil)
	ch, cancel := o.Subscribe()
	defer cancel()

	o.Pause(t.Context())
	o.Pause(t.Context())
	o.Pause(t.Context())
	if !o.IsPaused() {
		t.Fatal("Pause should leave IsPaused true")
	}
	o.Resume(t.Context())
	o.Resume(t.Context())
	if o.IsPaused() {
		t.Fatal("Resume should leave IsPaused false")
	}

	// Drain events: expect exactly one paused + one resumed.
	got := map[string]int{}
	for done := false; !done; {
		select {
		case ev := <-ch:
			got[ev.Type]++
		case <-time.After(50 * time.Millisecond):
			done = true
		}
	}
	if got["reconciler_paused"] != 1 || got["reconciler_resumed"] != 1 {
		t.Fatalf("want exactly one paused + one resumed event; got %+v", got)
	}
}

// TestHealthReportsPaused asserts the Paused flag shows up in the
// HealthStatus snapshot the control plane reads.
func TestHealthReportsPaused(t *testing.T) {
	o := New(baseConfig(), nil, nil, nil, nil)
	if h := o.Health(t.Context()); h.Paused {
		t.Fatal("fresh orchestrator should report Paused=false")
	}
	o.Pause(t.Context())
	if h := o.Health(t.Context()); !h.Paused {
		t.Fatal("after Pause, Health should report Paused=true")
	}
	o.Resume(t.Context())
	if h := o.Health(t.Context()); h.Paused {
		t.Fatal("after Resume, Health should report Paused=false")
	}
}
