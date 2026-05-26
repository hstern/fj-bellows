package control_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/gen/fjbellows/control/v1/controlv1connect"
	"github.com/hstern/fj-bellows/internal/control"
	mockctl "github.com/hstern/fj-bellows/internal/control/mock"
)

func TestLoadToken_TrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok")
	if err := os.WriteFile(path, []byte("  s3cr3t\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := control.LoadToken(path)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatalf("want s3cr3t got %q", got)
	}
}

func TestLoadToken_EmptyFileIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	if err := os.WriteFile(path, []byte("   \n\t\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := control.LoadToken(path); err == nil {
		t.Fatal("empty token file should error; got nil")
	}
}

func TestLoadToken_MissingFileIsError(t *testing.T) {
	if _, err := control.LoadToken(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing token file should error; got nil")
	}
}

func TestIsLoopbackBind(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9876", true},
		{"127.1.2.3:9876", true},
		{"localhost:9876", true},
		{"[::1]:9876", true},
		{"0.0.0.0:9876", false},
		{"10.0.0.5:9876", false},
		{"[fe80::1]:9876", false},
		{":9876", false}, // empty host binds all interfaces
		{"not-a-host-port", false},
		{"", false},
	} {
		got := control.IsLoopbackBind(tc.addr)
		if got != tc.want {
			t.Errorf("IsLoopbackBind(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// newAuthedServer wires a server with the bearer token enforced, returning
// an httptest.Server (HTTP/2 over TLS so streaming works) plus a Connect
// client that does NOT send the token by default.
func newAuthedServer(t *testing.T, backend control.Backend, token string) (*httptest.Server, controlv1connect.ControlServiceClient) {
	t.Helper()
	srv := control.NewServer("127.0.0.1:0", backend, nil, control.WithBearerToken(token))
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.EnableHTTP2 = true
	hs.StartTLS()
	t.Cleanup(hs.Close)
	client := controlv1connect.NewControlServiceClient(hs.Client(), hs.URL)
	return hs, client
}

func TestAuth_MissingToken_RejectsConnectRPC(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	_, client := newAuthedServer(t, be, "s3cr3t")

	_, err := client.Health(t.Context(), connect.NewRequest(&controlv1.HealthRequest{}))
	if err == nil {
		t.Fatal("unauthenticated request should error; got nil")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want CodeUnauthenticated, got %v", err)
	}
	if be.HealthCalls() != 0 {
		t.Fatalf("backend should not have been called; calls=%d", be.HealthCalls())
	}
}

func TestAuth_WrongToken_Rejects(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	hs, _ := newAuthedServer(t, be, "s3cr3t")

	// Connect client with the wrong token via header interceptor.
	wrongClient := controlv1connect.NewControlServiceClient(
		hs.Client(), hs.URL,
		connect.WithClientOptions(
			connect.WithInterceptors(headerInterceptor("Bearer wrong")),
		),
	)
	_, err := wrongClient.Health(t.Context(), connect.NewRequest(&controlv1.HealthRequest{}))
	if err == nil {
		t.Fatal("wrong-token request should error; got nil")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want CodeUnauthenticated, got %v", err)
	}
}

func TestAuth_CorrectToken_Allows(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	hs, _ := newAuthedServer(t, be, "s3cr3t")

	authedClient := controlv1connect.NewControlServiceClient(
		hs.Client(), hs.URL,
		connect.WithClientOptions(
			connect.WithInterceptors(headerInterceptor("Bearer s3cr3t")),
		),
	)
	resp, err := authedClient.Health(t.Context(), connect.NewRequest(&controlv1.HealthRequest{}))
	if err != nil {
		t.Fatalf("authed request should succeed: %v", err)
	}
	if !resp.Msg.Healthy {
		t.Fatalf("backend reply: %+v", resp.Msg)
	}
}

func TestAuth_Healthz_Open_Even_When_TokenSet(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	hs, _ := newAuthedServer(t, be, "s3cr3t")

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/healthz", nil)
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200 got %d", resp.StatusCode)
	}
}

func TestAuth_Metrics_Open_Even_When_TokenSet(t *testing.T) {
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	be.SetPoolSnapshot(func() []control.WorkerView { return nil })
	be.SetCacheStatus(func(context.Context) *control.CacheStatus { return nil })
	hs, _ := newAuthedServer(t, be, "s3cr3t")

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/metrics", nil)
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200 got %d", resp.StatusCode)
	}
}

func TestAuth_NoToken_Passthrough(t *testing.T) {
	// Server constructed without WithBearerToken — every RPC is open.
	be := &mockctl.Backend{}
	be.SetHealth(func(context.Context) control.HealthStatus { return control.HealthStatus{Healthy: true} })
	_, client := newTestServer(t, be) // not newAuthedServer
	resp, err := client.Health(t.Context(), connect.NewRequest(&controlv1.HealthRequest{}))
	if err != nil {
		t.Fatalf("no-token server should accept anything: %v", err)
	}
	if !resp.Msg.Healthy {
		t.Fatalf("body: %+v", resp.Msg)
	}
}

// headerInterceptor returns a Connect interceptor that injects a fixed
// Authorization header on every outbound request.
func headerInterceptor(value string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", value)
			return next(ctx, req)
		}
	})
}
