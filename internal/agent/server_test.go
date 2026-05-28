package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/hstern/fj-bellows/gen/fjbellows/agent/v1"
	"github.com/hstern/fj-bellows/gen/fjbellows/agent/v1/agentv1connect"
)

func TestServer_HealthOverHTTP_NoAuth(t *testing.T) {
	t.Parallel()

	h := NewHandler("test", time.Now())
	srv := NewServer("127.0.0.1:0", h, nil)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := agentv1connect.NewAgentServiceClient(http.DefaultClient, ts.URL)
	resp, err := client.Health(context.Background(), connect.NewRequest(&agentv1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !resp.Msg.Ready {
		t.Errorf("ready = false, want true")
	}
	if resp.Msg.BuildVersion != "test" {
		t.Errorf("build_version = %q, want %q", resp.Msg.BuildVersion, "test")
	}
}

func TestServer_BearerAuth_RejectsMissingToken(t *testing.T) {
	t.Parallel()

	h := NewHandler("test", time.Now())
	srv := NewServer("127.0.0.1:0", h, nil, WithBearerToken("s3cret"))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := agentv1connect.NewAgentServiceClient(http.DefaultClient, ts.URL)
	_, err := client.Health(context.Background(), connect.NewRequest(&agentv1.HealthRequest{}))
	if err == nil {
		t.Fatal("Health succeeded without token, want unauthenticated")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want CodeUnauthenticated; err = %v", connect.CodeOf(err), err)
	}
}

func TestServer_BearerAuth_AcceptsCorrectToken(t *testing.T) {
	t.Parallel()

	h := NewHandler("test", time.Now())
	srv := NewServer("127.0.0.1:0", h, nil, WithBearerToken("s3cret"))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	interceptor := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer s3cret")
			return next(ctx, req)
		}
	})
	client := agentv1connect.NewAgentServiceClient(http.DefaultClient, ts.URL, connect.WithInterceptors(interceptor))
	resp, err := client.Health(context.Background(), connect.NewRequest(&agentv1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !resp.Msg.Ready {
		t.Errorf("ready = false, want true")
	}
}

func TestServer_BearerAuth_RejectsWrongToken(t *testing.T) {
	t.Parallel()

	h := NewHandler("test", time.Now())
	srv := NewServer("127.0.0.1:0", h, nil, WithBearerToken("right"))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	interceptor := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer wrong")
			return next(ctx, req)
		}
	})
	client := agentv1connect.NewAgentServiceClient(http.DefaultClient, ts.URL, connect.WithInterceptors(interceptor))
	_, err := client.Health(context.Background(), connect.NewRequest(&agentv1.HealthRequest{}))
	if err == nil {
		t.Fatal("Health succeeded with wrong token, want unauthenticated")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}
}

func TestServer_HealthzOpenWithoutToken(t *testing.T) {
	t.Parallel()

	h := NewHandler("test", time.Now())
	srv := NewServer("127.0.0.1:0", h, nil, WithBearerToken("s3cret"))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// No Authorization header; /healthz must still answer 200.
	resp, err := http.Get(ts.URL + "/healthz") //nolint:noctx // test
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestServer_Run_ShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()

	h := NewHandler("test", time.Now())
	srv := NewServer("127.0.0.1:0", h, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Give the listener time to bind. We don't have a sync hook for that
	// here; in production main.go relies on systemd's Type=notify. For the
	// test, a tiny sleep is acceptable to ensure Serve has started.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of context cancel")
	}
}

func TestLoadToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	tests := []struct {
		name    string
		content string
		want    string
		wantErr string
	}{
		{name: "trimmed", content: "  abc\n", want: "abc"},
		{name: "bare", content: "xyz", want: "xyz"},
		{name: "empty", content: "", wantErr: "empty"},
		{name: "whitespace-only", content: "   \n  ", wantErr: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := filepath.Join(dir, tt.name)
			if err := os.WriteFile(p, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadToken(p)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadToken: got nil error, want one containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("LoadToken err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadToken: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadToken_Missing(t *testing.T) {
	t.Parallel()
	_, err := LoadToken(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("LoadToken: got nil error, want one for missing file")
	}
}
