package agent

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/hstern/fj-bellows/gen/fjbellows/agent/v1"
)

func TestHealth_PopulatesProcessFacts(t *testing.T) {
	t.Parallel()

	anchor := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	h := NewHandler("v0.5.0", anchor)

	resp, err := h.Health(context.Background(), connect.NewRequest(&agentv1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	got := resp.Msg

	if !got.Ready {
		t.Errorf("ready = false, want true")
	}
	if got.BuildVersion != "v0.5.0" {
		t.Errorf("build_version = %q, want %q", got.BuildVersion, "v0.5.0")
	}
	if got.StartedAt == nil {
		t.Fatal("started_at is nil")
	}
	if !got.StartedAt.AsTime().Equal(anchor) {
		t.Errorf("started_at = %v, want %v", got.StartedAt.AsTime(), anchor)
	}
	// hostname comes from os.Hostname(); we don't assert a specific value,
	// only that the field exists (it may legitimately be empty if the call
	// failed in this environment).
	_ = got.Hostname
}

func TestHealth_DevVersionDefault(t *testing.T) {
	t.Parallel()

	h := NewHandler("dev", time.Now())
	resp, err := h.Health(context.Background(), connect.NewRequest(&agentv1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if resp.Msg.BuildVersion != "dev" {
		t.Errorf("build_version = %q, want %q", resp.Msg.BuildVersion, "dev")
	}
}
