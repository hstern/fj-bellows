package control_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/internal/control"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

func TestGetCache_NotConfigured_AnswersPresentFalse(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })

	_, client := newTestServer(t, be)
	resp, err := client.GetCache(t.Context(), connect.NewRequest(&controlv1.GetCacheRequest{}))
	if err != nil {
		t.Fatalf("GetCache rpc: %v", err)
	}
	if resp.Msg.Present {
		t.Fatal("unconfigured cache must answer present=false")
	}
	if resp.Msg.LinodeId != 0 || resp.Msg.VmState != "" {
		t.Fatalf("unconfigured cache should zero remaining fields: %+v", resp.Msg)
	}
}

func TestGetCache_PopulatedAndRunning(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetCacheStatus(func(context.Context) *control.CacheStatus {
		return &control.CacheStatus{
			Present:         true,
			AdoptedExisting: false,
			LinodeID:        98765432,
			VPCIP:           "10.0.0.4",
			BucketRegion:    "us-ord",
			BucketLabel:     "fjb-cache-test",
			VMState:         "running",
		}
	})

	_, client := newTestServer(t, be)
	resp, err := client.GetCache(t.Context(), connect.NewRequest(&controlv1.GetCacheRequest{}))
	if err != nil {
		t.Fatalf("GetCache rpc: %v", err)
	}
	m := resp.Msg
	if !m.Present {
		t.Fatal("present should be true")
	}
	if m.LinodeId != 98765432 {
		t.Fatalf("LinodeId: want 98765432 got %d", m.LinodeId)
	}
	if m.VpcIp != "10.0.0.4" || m.BucketRegion != "us-ord" || m.BucketLabel != "fjb-cache-test" {
		t.Fatalf("bucket/IP fields: %+v", m)
	}
	if m.VmState != "running" {
		t.Fatalf("VmState: want running got %q", m.VmState)
	}
	if be.CacheStatusCalls() != 1 {
		t.Fatalf("CacheStatus calls: want 1 got %d", be.CacheStatusCalls())
	}
}
