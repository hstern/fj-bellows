package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/hstern/fj-bellows/gen/fjbellows/control/v1/controlv1connect"
)

// Server is the operator-facing control plane HTTP server.
//
// One http.Server multiplexes:
//   - ConnectRPC handlers (Connect/JSON, gRPC, gRPC-Web) at /<package>.<Service>/<Method>
//   - Plain HTTP /healthz so k8s probes and `curl --fail` work without Connect
//
// /metrics will join the same mux in PR5.
type Server struct {
	listen string
	srv    *http.Server
	log    *slog.Logger
}

// NewServer builds the server but does not start it.
// listen is a host:port suitable for net.Listen("tcp", ...).
func NewServer(listen string, backend Backend, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()

	path, handler := controlv1connect.NewControlServiceHandler(&apiHandler{b: backend})
	mux.Handle(path, handler)
	mux.HandleFunc("/healthz", plainHealthz(backend))

	// Enable HTTP/2 cleartext so gRPC clients can speak h2c over the
	// loopback-bound socket. Connect/JSON over HTTP/1.1 still works.
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetUnencryptedHTTP2(true)

	return &Server{
		listen: listen,
		log:    log,
		srv: &http.Server{
			Handler:           mux,
			Protocols:         &protos,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
}

// Handler returns the underlying mux for tests that want to mount the server
// behind httptest.NewServer without binding a real TCP port. Production code
// should call Run, which wires the same handler into an http.Server with
// HTTP/2 cleartext enabled.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

// Run binds the listener and serves until ctx is cancelled, at which point it
// initiates a short graceful shutdown.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.listen)
	if err != nil {
		return fmt.Errorf("control listen %s: %w", s.listen, err)
	}
	s.log.Info("control plane listening", "addr", ln.Addr().String())

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
			s.log.Error("control plane shutdown", "err", err)
		}
		return <-serveErr
	case err := <-serveErr:
		return err
	}
}
