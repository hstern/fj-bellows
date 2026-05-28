// Command fjbagent is the small daemon that runs on every fj-bellows worker
// and cache VM. It exposes ConnectRPC services the orchestrator dials into
// over the WG fabric — Health (FJB-94), and later Exec (FJB-93) and
// reachability probes (FJB-97).
//
// Listen address depends on the role:
//   - cache:  the cache's own WG inner address (e.g. 100.64.0.2:9001)
//   - worker: the worker's VPC IP (e.g. 10.0.0.5:9001); the orchestrator
//     reaches it through the cache-gateway routing FJB-54 set up
//
// Bind-address selection is the operator's job via -listen (typically set
// by the systemd unit from a cloud-init-substituted value). The agent
// itself doesn't care — it binds whatever it's told.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hstern/fj-bellows/internal/agent"
)

// version is the build version, stamped at link time via
//
//	-ldflags "-X main.version=<git describe output>"
//
// Defaults to "dev" for local builds.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fjbagent: %v\n", err)
		os.Exit(1)
	}
}

// run does the real work in a function with no os.Exit so deferred cleanup
// (the signal-context cancel) actually executes on error paths. main is a
// thin wrapper that translates the returned error into an exit code.
func run() error {
	listen := flag.String("listen", "127.0.0.1:9001", "host:port to bind the agent's ConnectRPC server (typically the target's VPC IP or WG inner address)")
	tokenFile := flag.String("token-file", "", "path to the bearer-token file (mode 0600); empty disables auth (test-only)")
	logLevel := flag.String("log-level", "info", "slog level: debug|info|warn|error")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(*logLevel),
	}))

	opts := []agent.Option{}
	if *tokenFile != "" {
		tok, err := agent.LoadToken(*tokenFile)
		if err != nil {
			return err
		}
		opts = append(opts, agent.WithBearerToken(tok))
	} else {
		log.Warn("agent running without bearer-token auth; do not use in production")
	}

	h := agent.NewHandler(version, time.Now())
	srv := agent.NewServer(*listen, h, log, opts...)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	return srv.Run(ctx)
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
