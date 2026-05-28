package agent

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hstern/fj-bellows/gen/fjbellows/agent/v1/agentv1connect"
)

// Server is the agent's HTTP front door. One http.Server multiplexes:
//   - ConnectRPC handlers at /<package>.<Service>/<Method>
//   - Plain HTTP /healthz for sd_notify-less readiness checks (curl --fail
//     style; not the gated readiness probe — that's AgentService.Health).
//
// The wire is HTTP/1.1 + HTTP/2 cleartext; encryption + authentication of
// the link is WG's responsibility, and on workers the cache↔worker leg is
// on the private VPC subnet behind Linode's VPC isolation.
type Server struct {
	listen string
	srv    *http.Server
	log    *slog.Logger
}

// Option configures a Server at construction time.
type Option func(*config)

type config struct {
	token string
}

// WithBearerToken enforces an Authorization: Bearer <token> header on every
// Connect RPC. The plain HTTP /healthz shim stays open so a sysadmin can
// curl the agent without the token. Empty token disables the middleware
// — useful for unit tests; production deployments always set it.
func WithBearerToken(token string) Option {
	return func(c *config) { c.token = token }
}

// NewServer builds the server but does not start it. listen is a host:port
// suitable for net.Listen("tcp", ...). The handler argument owns the
// process-scope health state.
func NewServer(listen string, h *Handler, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.Default()
	}
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}

	mux := http.NewServeMux()

	path, connectHandler := agentv1connect.NewAgentServiceHandler(h)
	mux.Handle(path, connectHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetUnencryptedHTTP2(true)

	return &Server{
		listen: listen,
		log:    log,
		srv: &http.Server{
			Handler:           bearerAuth(mux, cfg.token),
			Protocols:         &protos,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
}

// Handler returns the underlying mux for tests that want to mount the server
// behind httptest.NewServer without binding a real TCP port.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

// Run binds the listener and serves until ctx is cancelled, at which point
// it initiates a short graceful shutdown.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.listen)
	if err != nil {
		return fmt.Errorf("agent listen %s: %w", s.listen, err)
	}
	s.log.Info("agent listening", "addr", ln.Addr().String())

	serveErr := make(chan error, 1)
	go func() {
		err := s.srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			s.log.Error("agent shutdown", "err", err)
		}
		return <-serveErr
	case err := <-serveErr:
		return err
	}
}

// bearerAuth wraps next to require Authorization: Bearer <token> on Connect
// RPC paths. /healthz stays open. Empty token disables the middleware.
func bearerAuth(next http.Handler, token string) http.Handler {
	if token == "" {
		return next
	}
	expected := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="fjbagent"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthenticated"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LoadToken reads a single-line bearer token from path. Empty files and
// missing files are errors so the operator can't accidentally end up with
// the empty-token disable-auth branch by leaving the file blank.
func LoadToken(path string) (string, error) {
	//nolint:gosec // G304: path comes from operator-supplied -token-file flag, not user input.
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read agent token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("agent token file is empty")
	}
	return tok, nil
}
