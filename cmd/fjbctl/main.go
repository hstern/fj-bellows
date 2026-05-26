// Command fjbctl is the operator-facing CLI for a running fj-bellows daemon.
// It speaks Connect/JSON over HTTP to the daemon's control plane (see the
// internal/control package) and renders the responses in either a sorted
// human format or raw JSON.
//
// The first subcommand is `info`, which calls ProviderInfo (FJB-31) and
// dumps the provider's operator-debug key/value map. Subsequent commands
// (workers, cache, reconcile, …) land on the same dispatch shape.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"connectrpc.com/connect"

	"github.com/hstern/fj-bellows/gen/fjbellows/control/v1/controlv1connect"
)

// usage lists the subcommands fjbctl understands. Kept here (not on a
// subcommand-by-subcommand basis) so `fjbctl -h` shows the whole surface
// in one place.
const usage = `usage: fjbctl [-server URL] [-token-file PATH] [-json] <command> [args]

Commands:
  info        Show the provider's operator-debug key/value map (ProviderInfo).

Global flags:
  -server URL        Daemon control plane address (default http://127.0.0.1:9876)
  -token-file PATH   Bearer token file (mode 0600); required on non-loopback binds
  -json              Emit raw JSON instead of the default sorted text view
`

// commonFlags holds the global flags every fjbctl subcommand respects. They
// are parsed before subcommand dispatch so each subcommand sees the same
// shape regardless of how the operator interleaves -flag / command order.
type commonFlags struct {
	server    string
	tokenFile string
	json      bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fjbctl:", err)
		os.Exit(1)
	}
}

// run is the testable entrypoint: argv-style args plus injected io
// streams so unit tests can drive it without spawning a subprocess.
func run(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("fjbctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c commonFlags
	fs.StringVar(&c.server, "server", "http://127.0.0.1:9876", "daemon control plane address")
	fs.StringVar(&c.tokenFile, "token-file", "", "bearer token file (mode 0600)")
	fs.BoolVar(&c.json, "json", false, "emit raw JSON instead of the sorted text view")
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, usage)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("no command")
	}
	cmd, cmdArgs := rest[0], rest[1:]

	client, err := newClient(c)
	if err != nil {
		return err
	}

	switch cmd {
	case "info":
		return runInfo(cmdArgs, c, client, stdout, stderr)
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// newClient builds a Connect client for the daemon, wrapping the
// transport with a bearer-token interceptor when -token-file is set.
// The HTTP client is the stdlib default — Connect/JSON over HTTP/1.1
// is the wire shape every fjbctl call uses today; gRPC + HTTP/2 are
// available via the same generated client if a future use needs them.
func newClient(c commonFlags) (controlv1connect.ControlServiceClient, error) {
	var opts []connect.ClientOption
	if c.tokenFile != "" {
		tok, err := readToken(c.tokenFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts, connect.WithInterceptors(bearerInterceptor(tok)))
	}
	return controlv1connect.NewControlServiceClient(http.DefaultClient, c.server, opts...), nil
}

// readToken reads a one-line bearer token from path, trimming whitespace.
// Empty files (or files containing only whitespace) are an error so a
// misconfigured token-file fails fast instead of sending an empty
// Authorization header the daemon would reject as "no token".
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is operator-supplied
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}

// bearerInterceptor stamps Authorization: Bearer <token> onto every
// outgoing unary request. Server streams aren't wrapped because no
// fjbctl subcommand uses them yet; when StreamEvents/StreamLogs land,
// switch to connect.UnaryInterceptorFunc + a streaming variant.
func bearerInterceptor(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
}
