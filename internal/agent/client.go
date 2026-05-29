package agent

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/hstern/fj-bellows/gen/fjbellows/agent/v1/agentv1connect"
)

// DialContextFunc is the network-dial primitive a Client uses to open
// TCP connections. The orchestrator passes wg.Tunnel.DialContext so
// agent traffic rides the WireGuard netstack; tests pass an httptest-
// or bufconn-backed dialer. Signature matches net.Dialer.DialContext.
type DialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// ClientOptions configures NewClient.
type ClientOptions struct {
	// Addr is the agent's host:port. For workers, the worker's VPC IP +
	// the agent listen port; for cache, the cache's WG inner address +
	// the agent listen port.
	Addr string

	// Token is the per-deployment bearer secret presented on every RPC.
	// Empty disables the Authorization header — useful in tests.
	Token string

	// DialContext opens TCP connections. Required.
	DialContext DialContextFunc

	// RequestTimeout caps how long a single RPC may take. Defaults to
	// 30s when zero — long enough for a slow Health call on a busy
	// worker, short enough that callers don't sit forever on a dead
	// agent. Bidi-streaming RPCs (Exec) should use a per-stream
	// context instead.
	RequestTimeout time.Duration
}

// NewClient builds a ConnectRPC AgentServiceClient that dials through
// the supplied DialContext and presents the bearer token. The wire is
// HTTP/2 cleartext (h2c) — the orchestrator↔agent leg is already
// authenticated and encrypted by WG.
func NewClient(opts ClientOptions) agentv1connect.AgentServiceClient {
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 30 * time.Second
	}

	httpClient := &http.Client{
		Transport: &http2.Transport{
			// AllowHTTP + DialTLSContext-on-http2 over a plain TCP
			// dialer is how Connect speaks h2c. We pretend tls is
			// configured but the DialTLS just opens a TCP conn.
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return opts.DialContext(ctx, network, addr)
			},
		},
		Timeout: opts.RequestTimeout,
	}

	connectOpts := []connect.ClientOption{}
	if opts.Token != "" {
		connectOpts = append(connectOpts, connect.WithInterceptors(bearerInterceptor(opts.Token)))
	}

	return agentv1connect.NewAgentServiceClient(httpClient, "http://"+opts.Addr, connectOpts...)
}

// bearerInterceptor attaches `Authorization: Bearer <token>` to every
// outgoing request and stream.
func bearerInterceptor(token string) connect.Interceptor {
	auth := "Bearer " + token
	return interceptorFunc{
		unary: func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set("Authorization", auth)
				return next(ctx, req)
			}
		},
		streamClient: func(next connect.StreamingClientFunc) connect.StreamingClientFunc {
			return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
				conn := next(ctx, spec)
				conn.RequestHeader().Set("Authorization", auth)
				return conn
			}
		},
	}
}

// interceptorFunc adapts plain funcs into connect.Interceptor without
// implementing the full type for every middleware.
type interceptorFunc struct {
	unary        func(connect.UnaryFunc) connect.UnaryFunc
	streamClient func(connect.StreamingClientFunc) connect.StreamingClientFunc
}

func (f interceptorFunc) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	if f.unary == nil {
		return next
	}
	return f.unary(next)
}

func (f interceptorFunc) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	if f.streamClient == nil {
		return next
	}
	return f.streamClient(next)
}

func (f interceptorFunc) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
