package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"connectrpc.com/connect"

	"github.com/hstern/fj-bellows/gen/fjbellows/control/v1/controlv1connect"
)

const (
	defaultListen = "127.0.0.1:9876"
	envListen     = "FJBCTL_LISTEN"
	//nolint:gosec // G101: env var NAME, not a token value.
	envTokenFile = "FJBCTL_TOKEN_FILE"
)

// commonFlags holds the flags every subcommand carries: where to connect,
// optional bearer token, and JSON-vs-human output. Subcommands embed this
// struct in their own FlagSet so help output is consistent.
type commonFlags struct {
	listen    string
	tokenFile string
	json      bool
}

// listenURL converts a host:port to a base URL the generated Connect client
// can dial. Plain HTTP (h2c on the wire when the server enables
// UnencryptedHTTP2); TLS is the operator's reverse-proxy job, not ours.
// A fully-qualified URL (http:// or https://) is passed through unchanged,
// so tests pointing at httptest servers Just Work.
func (c commonFlags) listenURL() string {
	if strings.HasPrefix(c.listen, "http://") || strings.HasPrefix(c.listen, "https://") {
		return c.listen
	}
	return "http://" + c.listen
}

// readToken returns the operator's bearer token from the configured file, or
// an empty string when no file is configured. Trims whitespace; an empty
// (whitespace-only) file is an error so misconfig fails fast.
func (c commonFlags) readToken() (string, error) {
	if c.tokenFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", c.tokenFile, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", c.tokenFile)
	}
	return tok, nil
}

// client builds the generated Connect client with a bearer-token interceptor
// if a token file was configured. Reads listen/token-file from the operator's
// flags falling back to env vars, falling back to the loopback default.
func (c commonFlags) client() (controlv1connect.ControlServiceClient, error) {
	if c.listen == "" {
		if v := os.Getenv(envListen); v != "" {
			c.listen = v
		} else {
			c.listen = defaultListen
		}
	}
	if c.tokenFile == "" {
		c.tokenFile = os.Getenv(envTokenFile)
	}
	token, err := c.readToken()
	if err != nil {
		return nil, err
	}
	var opts []connect.ClientOption
	if token != "" {
		opts = append(opts, connect.WithInterceptors(bearerInterceptor(token)))
	}
	return controlv1connect.NewControlServiceClient(http.DefaultClient, c.listenURL(), opts...), nil
}

// bearerInterceptor injects Authorization: Bearer <token> on every outbound
// request. Mirrors the server-side enforcement in internal/control/auth.go.
func bearerInterceptor(token string) connect.Interceptor {
	header := "Bearer " + token
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", header)
			return next(ctx, req)
		}
	})
}

// fmtErr writes the error and returns 1 — the canonical "rpc failed" exit
// code subcommands use.
func fmtErr(stderr io.Writer, err error) int {
	outf(stderr, "fjbctl: %v\n", err)
	return 1
}
