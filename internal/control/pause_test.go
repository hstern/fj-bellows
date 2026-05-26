package control_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/gen/fjbellows/control/v1/controlv1connect"
	"github.com/hstern/fj-bellows/internal/control"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

// pauseResumeCase describes one of {Pause, Resume} — the RPC client call,
// the backend Set* wiring, and the *Calls counter accessor. Sharing the
// table between the disabled-by-default and happy-path subtests keeps the
// dupl linter happy and pins the two verbs to identical handler semantics.
type pauseResumeCase struct {
	name  string
	call  func(context.Context, controlv1connect.ControlServiceClient) error
	set   func(*mockctl.Backend)
	calls func(*mockctl.Backend) int
}

func pauseResumeCases() []pauseResumeCase {
	return []pauseResumeCase{
		{
			name: "Pause",
			call: func(ctx context.Context, c controlv1connect.ControlServiceClient) error {
				_, err := c.Pause(ctx, connect.NewRequest(&controlv1.PauseRequest{}))
				return err
			},
			set:   func(b *mockctl.Backend) { b.SetPause(func(context.Context) {}) },
			calls: (*mockctl.Backend).PauseCalls,
		},
		{
			name: "Resume",
			call: func(ctx context.Context, c controlv1connect.ControlServiceClient) error {
				_, err := c.Resume(ctx, connect.NewRequest(&controlv1.ResumeRequest{}))
				return err
			},
			set:   func(b *mockctl.Backend) { b.SetResume(func(context.Context) {}) },
			calls: (*mockctl.Backend).ResumeCalls,
		},
	}
}

// TestPauseResume_RPC pins the FJB-27 verbs to the FJB-26 gate semantics.
// Both subtests run the same matrix over {Pause, Resume}: with
// -enable-control-writes unset, the daemon returns CodePermissionDenied
// without touching the backend; with the flag set, the backend is invoked
// exactly once.
func TestPauseResume_RPC(t *testing.T) {
	for _, tc := range pauseResumeCases() {
		t.Run(tc.name+"_DisabledByDefault", func(t *testing.T) {
			be := &mockctl.Backend{}
			tc.set(be)
			_, client := newTestServer(t, be) // writes off
			err := tc.call(t.Context(), client)
			if err == nil {
				t.Fatalf("%s without writes-enabled should error", tc.name)
			}
			var ce *connect.Error
			if !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
				t.Fatalf("want CodePermissionDenied, got %v", err)
			}
			if n := tc.calls(be); n != 0 {
				t.Fatalf("backend should not have been called; calls=%d", n)
			}
		})
		t.Run(tc.name+"_HappyPath", func(t *testing.T) {
			be := &mockctl.Backend{}
			tc.set(be)
			_, client := newWritesServer(t, be, true)
			if err := tc.call(t.Context(), client); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if n := tc.calls(be); n != 1 {
				t.Fatalf("%s calls: want 1 got %d", tc.name, n)
			}
		})
	}
}

// TestHealth_RPC_PropagatesPaused pins the new Paused bool round-trips through
// the Health RPC.
func TestHealth_RPC_PropagatesPaused(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus {
		return control.HealthStatus{Healthy: true, Paused: true}
	})
	_, client := newTestServer(t, be)
	resp, err := client.Health(t.Context(), connect.NewRequest(&controlv1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !resp.Msg.Paused {
		t.Fatal("want Paused=true on response")
	}
}
