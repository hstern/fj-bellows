package control_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/internal/control"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

func TestGetConfig_RPC_ReturnsRedactedYAML(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetGetConfig(func(context.Context) (string, string) {
		return "forgejo:\n  token: <redacted>\n", "/etc/fj-bellows/config.yaml"
	})
	_, client := newTestServer(t, be)
	resp, err := client.GetConfig(t.Context(), connect.NewRequest(&controlv1.GetConfigRequest{}))
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if resp.Msg.ConfigPath != "/etc/fj-bellows/config.yaml" {
		t.Errorf("ConfigPath = %q", resp.Msg.ConfigPath)
	}
	if resp.Msg.Yaml != "forgejo:\n  token: <redacted>\n" {
		t.Errorf("Yaml = %q", resp.Msg.Yaml)
	}
	if be.GetConfigCalls() != 1 {
		t.Errorf("GetConfig calls: want 1 got %d", be.GetConfigCalls())
	}
}

func TestGetConfig_RPC_DoesNotRequireWritesEnabled(t *testing.T) {
	// Read-only RPC; must answer even when -enable-control-writes is off
	// (default in newTestServer). FJB-28 explicitly contrasts GetConfig
	// (read-only) with ReloadConfig (gated).
	be := &mockctl.Backend{}
	be.SetGetConfig(func(context.Context) (string, string) {
		return "{}", "/p"
	})
	_, client := newTestServer(t, be)
	if _, err := client.GetConfig(t.Context(), connect.NewRequest(&controlv1.GetConfigRequest{})); err != nil {
		t.Fatalf("GetConfig should not be gated: %v", err)
	}
}

func TestReloadConfig_RPC_DisabledByDefault(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetReloadConfig(func(context.Context) ([]string, error) { return nil, nil })
	_, client := newTestServer(t, be) // writes off
	_, err := client.ReloadConfig(t.Context(), connect.NewRequest(&controlv1.ReloadConfigRequest{}))
	if err == nil {
		t.Fatal("ReloadConfig without writes-enabled should error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied, got %v", err)
	}
	if be.ReloadConfigCalls() != 0 {
		t.Fatalf("backend should not have been called; calls=%d", be.ReloadConfigCalls())
	}
}

func TestReloadConfig_RPC_HappyPath(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetReloadConfig(func(context.Context) ([]string, error) {
		return []string{"poll.interval", "scale.max"}, nil
	})
	_, client := newWritesServer(t, be, true)
	resp, err := client.ReloadConfig(t.Context(), connect.NewRequest(&controlv1.ReloadConfigRequest{}))
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	want := []string{"poll.interval", "scale.max"}
	if !reflect.DeepEqual(resp.Msg.ChangedFields, want) {
		t.Errorf("ChangedFields:\nwant %v\n got %v", want, resp.Msg.ChangedFields)
	}
}

func TestReloadConfig_RPC_NoChangesReturnsEmpty(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetReloadConfig(func(context.Context) ([]string, error) {
		return nil, nil
	})
	_, client := newWritesServer(t, be, true)
	resp, err := client.ReloadConfig(t.Context(), connect.NewRequest(&controlv1.ReloadConfigRequest{}))
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if len(resp.Msg.ChangedFields) != 0 {
		t.Errorf("expected empty ChangedFields, got %v", resp.Msg.ChangedFields)
	}
}

func TestReloadConfig_RPC_NonHotFieldChangeMapsToFailedPrecondition(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetReloadConfig(func(context.Context) ([]string, error) {
		return nil, errors.New("reload rejected: tag changed")
	})
	_, client := newWritesServer(t, be, true)
	_, err := client.ReloadConfig(t.Context(), connect.NewRequest(&controlv1.ReloadConfigRequest{}))
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("want CodeFailedPrecondition, got %v", err)
	}
}

func TestReloadConfig_RPC_BackendInterfaceContract(t *testing.T) {
	// Pin that the handler does propagate to the backend exactly once
	// even on a no-op reload.
	be := &mockctl.Backend{}
	be.SetReloadConfig(func(context.Context) ([]string, error) { return nil, nil })
	_, client := newWritesServer(t, be, true)
	if _, err := client.ReloadConfig(t.Context(), connect.NewRequest(&controlv1.ReloadConfigRequest{})); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if be.ReloadConfigCalls() != 1 {
		t.Fatalf("ReloadConfig calls: want 1 got %d", be.ReloadConfigCalls())
	}
}

// Compile-time check that the mock satisfies the Backend interface; if
// we ever forget to wire a new method on either side, this fails first.
var _ control.Backend = (*mockctl.Backend)(nil)
