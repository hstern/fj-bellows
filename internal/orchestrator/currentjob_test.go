package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/forgejo"
	omock "github.com/hstern/fj-bellows/internal/orchestrator/mock"
	"github.com/hstern/fj-bellows/internal/provider"
	pmock "github.com/hstern/fj-bellows/internal/provider/mock"
)

// TestDispatchSetsAndClearsCurrentJob asserts the dispatch goroutine
// records the in-flight handle on Busy and clears it on the return to Idle.
// This is the property PoolSnapshot exposes to ListWorkers; without it the
// e2e harness can't tell which worker is serving which job.
func TestDispatchSetsAndClearsCurrentJob(t *testing.T) {
	prov := &pmock.Provider{
		ListFn: func(context.Context, string) ([]provider.Instance, error) {
			return []provider.Instance{{ID: "1", IPv4: "10.0.0.99", CreatedAt: time.Now()}}, nil
		},
	}
	jobs := &omock.JobSource{
		WaitingJobsFn: func(context.Context) ([]forgejo.WaitingJob, error) {
			return []forgejo.WaitingJob{{Handle: "job-xyz", Labels: []string{labelUbuntu}}}, nil
		},
	}

	// Block RunJob inside a "started" / "release" handshake so we can sample
	// the pool state mid-flight.
	started := make(chan struct{})
	release := make(chan struct{})
	disp := &omock.Dispatcher{
		RunJobFn: func(ctx context.Context, _, _ string, _ forgejo.Registration, _ forgejo.WaitingJob) error {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	}

	o := New(baseConfig(), prov, jobs, disp, nil)
	o.Reconcile(context.Background())

	// Wait for the goroutine to be in RunJob, then sample.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch goroutine never entered RunJob")
	}

	snap := o.PoolSnapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot length: want 1 got %d", len(snap))
	}
	if got := snap[0].CurrentJob; got != "job-xyz" {
		t.Fatalf("CurrentJob while busy: want job-xyz got %q", got)
	}
	if got := snap[0].State; got != string(StateBusy) {
		t.Fatalf("State while busy: want busy got %q", got)
	}

	close(release)

	waitFor(t, "CurrentJob clears on return to idle", func() bool {
		s := o.PoolSnapshot()
		return len(s) == 1 && s[0].CurrentJob == "" && s[0].State == string(StateIdle)
	})
}
