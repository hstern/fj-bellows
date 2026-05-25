package control_test

import (
	"testing"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/internal/control"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

func TestListWorkers_RPC_EmptyPool(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })

	_, client := newTestServer(t, be)
	resp, err := client.ListWorkers(t.Context(), connect.NewRequest(&controlv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatalf("ListWorkers rpc: %v", err)
	}
	if got := len(resp.Msg.Workers); got != 0 {
		t.Fatalf("workers: want 0 got %d", got)
	}
	if be.PoolSnapshotCalls() != 1 {
		t.Fatalf("PoolSnapshot calls: want 1 got %d", be.PoolSnapshotCalls())
	}
}

func TestListWorkers_RPC_PopulatedPool(t *testing.T) {
	created := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	lastBusy := created.Add(30 * time.Second)

	be := &mockctl.Backend{}
	be.SetPoolSnapshot(func() []control.WorkerView {
		return []control.WorkerView{
			{
				InstanceID: "linode-12345",
				State:      "busy",
				IP:         "203.0.113.5",
				CreatedAt:  created,
				LastBusy:   lastBusy,
				CurrentJob: "job-abc",
			},
			{
				InstanceID: "linode-67890",
				State:      "idle",
				IP:         "203.0.113.7",
				CreatedAt:  created.Add(-time.Minute),
				LastBusy:   lastBusy.Add(-time.Minute),
				// CurrentJob intentionally empty for idle nodes.
			},
		}
	})

	_, client := newTestServer(t, be)
	resp, err := client.ListWorkers(t.Context(), connect.NewRequest(&controlv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatalf("ListWorkers rpc: %v", err)
	}
	if got := len(resp.Msg.Workers); got != 2 {
		t.Fatalf("workers: want 2 got %d", got)
	}

	w0 := resp.Msg.Workers[0]
	if w0.InstanceId != "linode-12345" || w0.State != "busy" || w0.Ip != "203.0.113.5" {
		t.Fatalf("worker[0] core fields: %+v", w0)
	}
	if w0.CurrentJob != "job-abc" {
		t.Fatalf("worker[0].current_job: want job-abc got %q", w0.CurrentJob)
	}
	if got := w0.CreatedAt.AsTime(); !got.Equal(created) {
		t.Fatalf("worker[0].created_at: want %v got %v", created, got)
	}

	w1 := resp.Msg.Workers[1]
	if w1.State != "idle" {
		t.Fatalf("worker[1].state: want idle got %q", w1.State)
	}
	if w1.CurrentJob != "" {
		t.Fatalf("idle worker must have empty current_job; got %q", w1.CurrentJob)
	}
}
