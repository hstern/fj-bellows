package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// registerCommonFlags wires -listen / -token-file / -json onto a FlagSet.
// Subcommands call this on their own FlagSet to keep flag names uniform.
func registerCommonFlags(fs *flag.FlagSet, c *commonFlags) {
	fs.StringVar(&c.listen, "listen", "", "control plane address (host:port). Default 127.0.0.1:9876, override with $FJBCTL_LISTEN.")
	fs.StringVar(&c.tokenFile, "token-file", "", "bearer-token file for non-loopback deployments. Override with $FJBCTL_TOKEN_FILE.")
	fs.BoolVar(&c.json, "json", false, "emit the raw proto-JSON response instead of the human-readable rendering")
}

// printJSON marshals a proto.Message with stable field naming + indentation
// and writes it to stdout. Returns 0 on success, 1 on marshal failure.
func printJSON(stdout, stderr io.Writer, msg proto.Message) int {
	opts := protojson.MarshalOptions{Multiline: true, Indent: "  "}
	b, err := opts.Marshal(msg)
	if err != nil {
		return fmtErr(stderr, err)
	}
	_, _ = fmt.Fprintln(stdout, string(b))
	return 0
}

// contextWithTimeout is the default per-request deadline for unary
// subcommands. Streaming subcommands ignore this and live for as long as
// the connection holds.
func contextWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// Stdio write helpers that explicitly discard errors. Writing to a closed
// or full stdout is a CLI error case we can't usefully react to mid-command;
// these satisfy errcheck without scattering `_, _ =` at every callsite.
// Named `outf`/`outln` (not `printf`/`println`) so they don't shadow the
// builtin `println`.
func outf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func outln(w io.Writer, a ...any) {
	_, _ = fmt.Fprintln(w, a...)
}
