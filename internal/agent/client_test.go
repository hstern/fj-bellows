package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/hstern/fj-bellows/gen/fjbellows/agent/v1"
)

// newTestServerForClient starts an in-process agent server bound to
// 127.0.0.1:0 with HTTP/2 cleartext (h2c) so bidi-streaming RPCs work
// without TLS — same shape as production where the wire rides WG. The
// returned DialContext bypasses normal DNS and connects directly to
// the bound listener, mirroring how wg.Tunnel.DialContext will route in
// production.
func newTestServerForClient(t *testing.T, token string) (addr string, dial DialContextFunc, teardown func()) {
	t.Helper()
	h := NewHandler("test", time.Now())
	var opts []Option
	if token != "" {
		opts = append(opts, WithBearerToken(token))
	}
	srv := NewServer("127.0.0.1:0", h, nil, opts...)

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetUnencryptedHTTP2(true)
	hsrv := &http.Server{
		Handler:           srv.Handler(),
		Protocols:         &protos,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		err := hsrv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	host := ln.Addr().String()
	dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", host)
	}

	var once sync.Once
	teardown = func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = hsrv.Shutdown(ctx)
			<-serveErr
		})
	}
	return host, dial, teardown
}

func TestClient_Health_NoAuth(t *testing.T) {
	t.Parallel()
	host, dial, teardown := newTestServerForClient(t, "")
	defer teardown()

	client := NewClient(ClientOptions{
		Addr:        host,
		DialContext: dial,
	})
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

func TestClient_Health_BearerToken_OK(t *testing.T) {
	t.Parallel()
	host, dial, teardown := newTestServerForClient(t, "s3cret")
	defer teardown()

	client := NewClient(ClientOptions{
		Addr:        host,
		Token:       "s3cret",
		DialContext: dial,
	})
	resp, err := client.Health(context.Background(), connect.NewRequest(&agentv1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !resp.Msg.Ready {
		t.Errorf("ready = false, want true")
	}
}

func TestClient_Health_WrongToken_Rejected(t *testing.T) {
	t.Parallel()
	host, dial, teardown := newTestServerForClient(t, "right")
	defer teardown()

	client := NewClient(ClientOptions{
		Addr:        host,
		Token:       "wrong",
		DialContext: dial,
	})
	_, err := client.Health(context.Background(), connect.NewRequest(&agentv1.HealthRequest{}))
	if err == nil {
		t.Fatal("Health succeeded with wrong token, want CodeUnauthenticated")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}
}

// Confirm the bearer interceptor sets the header even on stream RPCs
// (Exec is bidi; the orchestrator side will use this client for it).
func TestClient_Exec_BearerToken_OK(t *testing.T) {
	t.Parallel()
	host, dial, teardown := newTestServerForClient(t, "s3cret")
	defer teardown()

	client := NewClient(ClientOptions{
		Addr:        host,
		Token:       "s3cret",
		DialContext: dial,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := client.Exec(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&agentv1.ShellMsg{Kind: &agentv1.ShellMsg_Open{
		Open: &agentv1.ExecOpen{Argv: []string{execTestArgvBin, "via-client-helper"}},
	}}); err != nil {
		t.Fatalf("Send Open: %v", err)
	}
	var got strings.Builder
	for {
		ev, err := stream.Receive()
		if err != nil {
			break
		}
		switch k := ev.GetKind().(type) {
		case *agentv1.ShellEvent_Stdout:
			got.Write(k.Stdout.GetData())
		case *agentv1.ShellEvent_Exit:
			if k.Exit.GetCode() != 0 {
				t.Errorf("exit = %d, want 0", k.Exit.GetCode())
			}
			if want := "via-client-helper\n"; got.String() != want {
				t.Errorf("stdout = %q, want %q", got.String(), want)
			}
			return
		}
	}
}

// Sanity: confirm we route via DialContext, not via the default dialer.
// We pass a DialContext that errors and assert the call fails with that
// error.
func TestClient_UsesDialContext(t *testing.T) {
	t.Parallel()
	sentinel := dialSentinelError("dial sentinel reached")
	client := NewClient(ClientOptions{
		Addr: "host-that-should-not-be-resolved-by-default-dialer:9001",
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, sentinel
		},
		RequestTimeout: 2 * time.Second,
	})
	_, err := client.Health(context.Background(), connect.NewRequest(&agentv1.HealthRequest{}))
	if err == nil {
		t.Fatal("Health succeeded; want sentinel error from DialContext")
	}
	if !strings.Contains(err.Error(), "dial sentinel reached") {
		t.Errorf("expected sentinel in error, got %v", err)
	}
}

type dialSentinelError string

func (e dialSentinelError) Error() string { return string(e) }

// Sanity: the helper does not import http blindly.
var _ = http.NoBody
