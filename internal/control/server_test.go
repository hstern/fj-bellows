package control_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/gen/fjbellows/control/v1/controlv1connect"
	"github.com/hstern/fj-bellows/internal/control"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

// newTestServer wires the same mux NewServer wires, but as an httptest.Server
// so we can hit it without binding a real port. It returns the URL prefix
// callers compose with /<package>.<Service>/<Method>.
func newTestServer(t *testing.T, backend control.Backend) (*httptest.Server, controlv1connect.ControlServiceClient) {
	t.Helper()
	srv := control.NewServer("127.0.0.1:0", backend, nil)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	client := controlv1connect.NewControlServiceClient(hs.Client(), hs.URL)
	return hs, client
}

func TestHealth_RPC_Unhealthy_OnFreshDaemon(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus {
		// Zero times → never ticked → unhealthy.
		return control.HealthStatus{Healthy: false}
	})

	_, client := newTestServer(t, be)
	resp, err := client.Health(t.Context(), connect.NewRequest(&controlv1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health rpc: %v", err)
	}
	if resp.Msg.Healthy {
		t.Fatal("expected unhealthy on zero-timestamps")
	}
	if resp.Msg.LastTickAt != nil {
		t.Fatalf("zero time should omit Timestamp; got %v", resp.Msg.LastTickAt)
	}
}

func TestHealth_RPC_Healthy_RoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus {
		return control.HealthStatus{
			Healthy:            true,
			LastTickAt:         now,
			LastProviderListAt: now.Add(-2 * time.Second),
			LastForgejoPollAt:  now.Add(-1 * time.Second),
		}
	})

	_, client := newTestServer(t, be)
	resp, err := client.Health(t.Context(), connect.NewRequest(&controlv1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health rpc: %v", err)
	}
	if !resp.Msg.Healthy {
		t.Fatal("expected healthy")
	}
	if got := resp.Msg.LastTickAt.AsTime(); !got.Equal(now) {
		t.Fatalf("LastTickAt: want %v got %v", now, got)
	}
	if be.HealthCalls() != 1 {
		t.Fatalf("expected exactly 1 Health call, got %d", be.HealthCalls())
	}
}

func TestHealthz_Plain_HTTP_200_When_Healthy(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus {
		return control.HealthStatus{Healthy: true, LastTickAt: time.Now()}
	})
	hs, _ := newTestServer(t, be)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/healthz", nil)
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200 got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v (raw: %s)", err, body)
	}
	if got["healthy"] != true {
		t.Fatalf("body healthy: %v", got)
	}
}

func TestHealthz_Plain_HTTP_503_When_Unhealthy(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus {
		return control.HealthStatus{Healthy: false}
	})
	hs, _ := newTestServer(t, be)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/healthz", nil)
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: want 503 got %d", resp.StatusCode)
	}
}

func TestConnect_JSON_Wire_Curl_Compatible(t *testing.T) {
	// The e2e harness will speak the Connect protocol with `curl -d '{}'`,
	// so verify that a plain HTTP/1.1 POST with content-type: application/json
	// to /<package>.<Service>/Health returns the expected JSON body. This pins
	// the wire format we're advertising to the harness.
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus {
		return control.HealthStatus{Healthy: true, LastTickAt: time.Now()}
	})
	hs, _ := newTestServer(t, be)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost,
		hs.URL+"/"+controlv1connect.ControlServiceName+"/Health",
		strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")

	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatalf("POST Health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: want 200 got %d (body: %s)", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v (raw: %s)", err, body)
	}
	if got["healthy"] != true {
		t.Fatalf("body healthy: %v", got)
	}
}
