// Package docker implements the provider.Provider interface against the local
// Docker daemon by shelling out to the `docker` CLI. This avoids importing the
// Docker Go SDK (which trips govulncheck GO-2026-4887, unfixable, and pulls a
// large dependency tree). Operators of this provider already require a working
// Docker install; requiring the `docker` binary on PATH is no extra cost.
package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// cli is the minimal surface both the provider and the exec dispatcher need
// from the docker CLI. It is an interface so unit tests can inject an
// in-memory fake and `go test` works without a docker binary or daemon.
type cli interface {
	// Run executes the docker subcommand with args, writes stdin to it if
	// non-nil, and returns combined stdout (stderr is captured into the
	// returned error on non-zero exit so the caller can surface diagnostics).
	Run(ctx context.Context, stdin io.Reader, args ...string) (stdout []byte, err error)
}

// execCLI is the production cli that shells out via os/exec.
type execCLI struct {
	bin string
}

// newExecCLI returns a cli that invokes the docker binary at bin (e.g.
// "docker" on PATH, or an absolute path).
func newExecCLI(bin string) *execCLI {
	return &execCLI{bin: bin}
}

// Run executes `<bin> <args...>`, feeding stdin if non-nil and returning
// stdout. On non-zero exit the returned error includes the captured stderr.
func (e *execCLI) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	// G204: args are constructed in-package from typed inputs (Spec.Tag,
	// container ID returned by docker, operator-supplied image/network).
	// bin is operator-supplied config that defaults to "docker"; treating it
	// as untrusted would block any configurability of the docker provider.
	//nolint:gosec // G204: docker subcommand args are internally constructed; bin is operator-config.
	cmd := exec.CommandContext(ctx, e.bin, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
		}
		return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}
