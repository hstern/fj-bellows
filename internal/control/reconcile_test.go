package control_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/internal/control"
	"github.com/hstern/fj-bellows/internal/control/events"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

func TestReconcile_RPC_HappyPath(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetKick(func(context.Context) (control.ReconcileResult, error) {
		return control.ReconcileResult{
			Provisioned: 1,
			Dispatched:  2,
			Reaped:      0,
			Adopted:     1,
			Dropped:     0,
		}, nil
	})

	_, client := newTestServer(t, be)
	resp, err := client.Reconcile(t.Context(), connect.NewRequest(&controlv1.ReconcileRequest{}))
	if err != nil {
		t.Fatalf("Reconcile rpc: %v", err)
	}
	m := resp.Msg
	if m.Provisioned != 1 || m.Dispatched != 2 || m.Adopted != 1 {
		t.Fatalf("counts: %+v", m)
	}
	if len(m.Errors) != 0 {
		t.Fatalf("Errors: %v", m.Errors)
	}
	if be.KickCalls() != 1 {
		t.Fatalf("Kick calls: want 1 got %d", be.KickCalls())
	}
}

func TestReconcile_RPC_BackendError_Surfaces(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetKick(func(context.Context) (control.ReconcileResult, error) {
		return control.ReconcileResult{}, errors.New("orchestrator not running")
	})

	_, client := newTestServer(t, be)
	_, err := client.Reconcile(t.Context(), connect.NewRequest(&controlv1.ReconcileRequest{}))
	if err == nil {
		t.Fatal("expected an error to surface")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInternal {
		t.Fatalf("expected CodeInternal, got %v", err)
	}
}

func TestStreamEvents_RPC_DeliversPublishedEvents(t *testing.T) {
	// Wire a real bus into the mock so the test exercises the same
	// fan-out path the orchestrator will use in production.
	bus := events.New()
	be := &mockctl.Backend{}
	be.SetSubscribe(bus.Subscribe)

	_, client := newTestServer(t, be)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	stream, err := client.StreamEvents(ctx, connect.NewRequest(&controlv1.StreamEventsRequest{}))
	if err != nil {
		t.Fatalf("StreamEvents open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// First message is the stream_opened sentinel that lets the open call
	// return immediately even on a quiet daemon. Confirm it; then publish
	// real events.
	if !stream.Receive() {
		t.Fatalf("sentinel Receive: %v", stream.Err())
	}
	if got := stream.Msg().Type; got != "stream_opened" {
		t.Fatalf("first message: want stream_opened got %q", got)
	}

	bus.Publish(events.Event{At: time.Now(), Type: "worker_provisioned", Attrs: map[string]string{"id": "vm-1"}})
	bus.Publish(events.Event{At: time.Now(), Type: "job_complete", Attrs: map[string]string{"id": "vm-1", "handle": "h1"}})

	if !stream.Receive() {
		t.Fatalf("first Receive: %v", stream.Err())
	}
	if got := stream.Msg().Type; got != "worker_provisioned" {
		t.Fatalf("event 1 type: want worker_provisioned got %q", got)
	}
	if stream.Msg().Attrs["id"] != "vm-1" {
		t.Fatalf("event 1 attrs: %+v", stream.Msg().Attrs)
	}

	if !stream.Receive() {
		t.Fatalf("second Receive: %v", stream.Err())
	}
	if got := stream.Msg().Type; got != "job_complete" {
		t.Fatalf("event 2 type: want job_complete got %q", got)
	}
}
