package control_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/gen/fjbellows/control/v1/controlv1connect"
	"github.com/hstern/fj-bellows/internal/control"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

// newWritesServer wires a control server with WithControlWrites(enable) on
// top of the standard test backend.
func newWritesServer(t *testing.T, backend control.Backend, enable bool) (*httptest.Server, controlv1connect.ControlServiceClient) {
	t.Helper()
	srv := control.NewServer("127.0.0.1:0", backend, nil, control.WithControlWrites(enable))
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.EnableHTTP2 = true
	hs.StartTLS()
	t.Cleanup(hs.Close)
	client := controlv1connect.NewControlServiceClient(hs.Client(), hs.URL)
	return hs, client
}

func TestForceReap_RPC_DisabledByDefault(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetForceReap(func(context.Context, string) error { return nil })
	_, client := newTestServer(t, be) // default: writes off
	_, err := client.ForceReap(t.Context(), connect.NewRequest(&controlv1.ForceReapRequest{InstanceId: "100"}))
	if err == nil {
		t.Fatal("ForceReap without writes-enabled should error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied, got %v", err)
	}
	if be.ForceReapCalls() != 0 {
		t.Fatalf("backend should not have been called; calls=%d", be.ForceReapCalls())
	}
}

func TestForceReap_RPC_HappyPath(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetForceReap(func(_ context.Context, id string) error {
		if id != "100" {
			t.Errorf("ForceReap got id=%q; want 100", id)
		}
		return nil
	})
	_, client := newWritesServer(t, be, true)
	_, err := client.ForceReap(t.Context(), connect.NewRequest(&controlv1.ForceReapRequest{InstanceId: "100"}))
	if err != nil {
		t.Fatalf("ForceReap: %v", err)
	}
	if be.ForceReapCalls() != 1 {
		t.Fatalf("ForceReap calls: want 1 got %d", be.ForceReapCalls())
	}
}

func TestForceReap_RPC_MissingInstanceIDIsInvalidArgument(t *testing.T) {
	be := &mockctl.Backend{}
	_, client := newWritesServer(t, be, true)
	_, err := client.ForceReap(t.Context(), connect.NewRequest(&controlv1.ForceReapRequest{}))
	if err == nil {
		t.Fatal("empty instance_id should error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument, got %v", err)
	}
}

// TestForceReap_RPC_ErrorMapping pins that "not in pool" surfaces as
// CodeNotFound and everything else as CodeInternal. Table-driven so adding
// a new mapping doesn't blow up the file with duplicated boilerplate.
func TestForceReap_RPC_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"not in pool", errors.New(`instance "ghost" not in pool`), connect.CodeNotFound},
		{"vanished", errors.New(`instance "100" vanished from pool`), connect.CodeNotFound},
		{"destroy failure", errors.New("destroy: api 500"), connect.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := &mockctl.Backend{}
			be.SetForceReap(func(context.Context, string) error { return tc.err })
			_, client := newWritesServer(t, be, true)
			_, err := client.ForceReap(t.Context(), connect.NewRequest(&controlv1.ForceReapRequest{InstanceId: "100"}))
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *connect.Error
			if !errors.As(err, &ce) || ce.Code() != tc.want {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestForceProvision_RPC_DisabledByDefault(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetForceProvision(func(context.Context) (string, error) { return "200", nil })
	_, client := newTestServer(t, be)
	_, err := client.ForceProvision(t.Context(), connect.NewRequest(&controlv1.ForceProvisionRequest{}))
	if err == nil {
		t.Fatal("ForceProvision without writes-enabled should error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied, got %v", err)
	}
	if be.ForceProvisionCalls() != 0 {
		t.Fatalf("backend should not have been called; calls=%d", be.ForceProvisionCalls())
	}
}

func TestForceProvision_RPC_ReturnsNewID(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetForceProvision(func(context.Context) (string, error) { return "200", nil })
	_, client := newWritesServer(t, be, true)
	resp, err := client.ForceProvision(t.Context(), connect.NewRequest(&controlv1.ForceProvisionRequest{}))
	if err != nil {
		t.Fatalf("ForceProvision: %v", err)
	}
	if resp.Msg.InstanceId != "200" {
		t.Fatalf("InstanceId: want 200 got %q", resp.Msg.InstanceId)
	}
}

func TestForceProvision_RPC_BackendErrorMapsToInternal(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetForceProvision(func(context.Context) (string, error) { return "", errors.New("out of quota") })
	_, client := newWritesServer(t, be, true)
	_, err := client.ForceProvision(t.Context(), connect.NewRequest(&controlv1.ForceProvisionRequest{}))
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInternal {
		t.Fatalf("want CodeInternal, got %v", err)
	}
}
