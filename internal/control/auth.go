package control

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

// LoadToken reads a single-line bearer token from path. Empties and missing
// files are errors so the operator can't accidentally end up with an empty
// allowed-token set (which would mean "no auth"). Whitespace around the token
// is trimmed.
func LoadToken(path string) (string, error) {
	//nolint:gosec // G304: path is operator-supplied via -control-token-file, not user input.
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read control token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("control token file is empty")
	}
	return tok, nil
}

// IsLoopbackBind reports whether a host:port listen string targets the
// loopback interface. Used by main.go to decide whether the bearer-token
// check is the deployment's auth boundary, and by NewServer to short-circuit
// the middleware when loopback alone suffices.
func IsLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Treat unparseable as "not loopback" — fail closed; the operator
		// gets the auth check if their bind is malformed.
		return false
	}
	if host == "localhost" || host == "" {
		// Empty host (":9876") binds all interfaces, NOT loopback. Only
		// "localhost" string maps to loopback in this branch.
		return host == "localhost"
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bearerAuth wraps an http.Handler to require an Authorization: Bearer
// <token> header on Connect RPC paths. The /healthz and /metrics plain-HTTP
// shims stay open so Prom scrapers and k8s probes don't need to carry the
// token. (The ticket calls out an optional read/write split — read RPCs open,
// write RPCs gated — but v1 keeps it simple: all Connect RPCs require the
// token.) Returns next unchanged when token == "" so callers can install the
// middleware unconditionally and let configuration choose.
func bearerAuth(next http.Handler, token string) http.Handler {
	if token == "" {
		return next
	}
	expected := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /healthz and /metrics are always open; they're consumed by
		// ecosystem tooling that doesn't speak Connect and can't carry
		// per-request bearer creds in practice.
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		// Constant-time compare so a remote attacker can't time-side-channel
		// the token byte-by-byte.
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="fjb-control"`)
			w.WriteHeader(http.StatusUnauthorized)
			// Tiny JSON body so curl --fail and Connect clients both see a
			// useful message. Connect maps HTTP 401 to CodeUnauthenticated
			// regardless of body, so the format is informational here.
			_, _ = w.Write([]byte(`{"error":"unauthenticated"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
