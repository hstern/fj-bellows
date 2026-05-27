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

// twoWorkerFixture is the canonical pool snapshot used by the
// _PopulatedPool / _BillingWindow tests below.
type twoWorkerFixture struct {
	created time.Time
	paidEnd time.Time
	reapAt  time.Time
	snap    []control.WorkerView
}

// twoWorkerPool returns the canonical two-worker fixture: one busy
// hourly-billed worker and one idle per-second worker, so both billing
// branches get exercised.
func twoWorkerPool() twoWorkerFixture {
	created := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	lastBusy := created.Add(30 * time.Second)
	paidEnd := created.Add(time.Hour)
	reapAt := created.Add(55 * time.Minute)
	return twoWorkerFixture{
		created: created,
		paidEnd: paidEnd,
		reapAt:  reapAt,
		snap: []control.WorkerView{
			{
				InstanceID:     "linode-12345",
				State:          "busy",
				IP:             "203.0.113.5",
				VPCIP:          "10.0.0.10",
				CreatedAt:      created,
				LastBusy:       lastBusy,
				CurrentJob:     "job-abc",
				PaidHourEndAt:  paidEnd,
				ReapEligibleAt: reapAt,
				BillingModel:   "hourly_round_up",
			},
			{
				InstanceID: "linode-67890",
				State:      "idle",
				IP:         "203.0.113.7",
				// VPCIP intentionally empty — covers the no-VPC path.
				CreatedAt:      created.Add(-time.Minute),
				LastBusy:       lastBusy.Add(-time.Minute),
				ReapEligibleAt: lastBusy.Add(-time.Minute).Add(5 * time.Minute),
				BillingModel:   "per_second",
				// CurrentJob intentionally empty for idle nodes.
				// PaidHourEndAt intentionally zero for per-second.
			},
		},
	}
}

func TestListWorkers_RPC_PopulatedPool(t *testing.T) {
	fx := twoWorkerPool()
	be := &mockctl.Backend{}
	be.SetPoolSnapshot(func() []control.WorkerView { return fx.snap })

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
	if got := w0.CreatedAt.AsTime(); !got.Equal(fx.created) {
		t.Fatalf("worker[0].created_at: want %v got %v", fx.created, got)
	}

	if w0.VpcIp != "10.0.0.10" {
		t.Fatalf("worker[0].vpc_ip: want 10.0.0.10 got %q", w0.VpcIp)
	}

	w1 := resp.Msg.Workers[1]
	if w1.State != "idle" {
		t.Fatalf("worker[1].state: want idle got %q", w1.State)
	}
	if w1.CurrentJob != "" {
		t.Fatalf("idle worker must have empty current_job; got %q", w1.CurrentJob)
	}
	if w1.VpcIp != "" {
		t.Fatalf("worker[1].vpc_ip: want empty (no VPC) got %q", w1.VpcIp)
	}
}

// TestListWorkers_RPC_BillingWindow covers FJB-30: the per-worker billing
// model + reap window + paid-hour boundary fields populated by the
// orchestrator's TeardownPolicy.Timing snapshot.
func TestListWorkers_RPC_BillingWindow(t *testing.T) {
	fx := twoWorkerPool()
	be := &mockctl.Backend{}
	be.SetPoolSnapshot(func() []control.WorkerView { return fx.snap })

	_, client := newTestServer(t, be)
	resp, err := client.ListWorkers(t.Context(), connect.NewRequest(&controlv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatalf("ListWorkers rpc: %v", err)
	}

	// Hourly-billed worker: all three fields populated.
	w0 := resp.Msg.Workers[0]
	if w0.BillingModel != "hourly_round_up" {
		t.Fatalf("worker[0].billing_model: want hourly_round_up got %q", w0.BillingModel)
	}
	if w0.PaidHourEndAt == nil || !w0.PaidHourEndAt.AsTime().Equal(fx.paidEnd) {
		t.Fatalf("worker[0].paid_hour_end_at: want %v got %v", fx.paidEnd, w0.PaidHourEndAt)
	}
	if w0.ReapEligibleAt == nil || !w0.ReapEligibleAt.AsTime().Equal(fx.reapAt) {
		t.Fatalf("worker[0].reap_eligible_at: want %v got %v", fx.reapAt, w0.ReapEligibleAt)
	}

	// Per-second worker: model + reap populated, paid-hour empty.
	w1 := resp.Msg.Workers[1]
	if w1.BillingModel != "per_second" {
		t.Fatalf("worker[1].billing_model: want per_second got %q", w1.BillingModel)
	}
	if w1.PaidHourEndAt != nil {
		t.Fatalf("per-second worker must have nil paid_hour_end_at; got %v", w1.PaidHourEndAt)
	}
	if w1.ReapEligibleAt == nil {
		t.Fatalf("per-second worker should populate reap_eligible_at; got nil")
	}
}
