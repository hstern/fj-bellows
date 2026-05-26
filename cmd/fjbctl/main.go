// Command fjbctl is the operator-facing CLI for the fj-bellows control plane.
// It speaks the ConnectRPC service defined in proto/fjbellows/control/v1
// against a running daemon's `-control-listen` address.
//
// Subcommands map 1:1 to RPCs:
//
//	fjbctl health     — readiness snapshot
//	fjbctl workers    — list workers; --watch redraws on each state event
//	fjbctl cache      — managed pull-through cache VM state
//	fjbctl reconcile  — drive a synchronous reconcile tick
//	fjbctl events     — stream state-transition events
//
// Output is human-readable by default; pass --json to any subcommand for the
// raw protobuf-JSON response.
package main

import (
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) < 1 {
		printUsage(stderr)
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		// Explicit help → stdout, exit 0 (Unix convention; `fjbctl help | less` works).
		printUsage(stdout)
		return 0
	}
	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "health":
		return cmdHealth(rest, stdout, stderr)
	case "workers":
		return cmdWorkers(rest, stdout, stderr)
	case "cache":
		return cmdCache(rest, stdout, stderr)
	case "reconcile":
		return cmdReconcile(rest, stdout, stderr)
	case "events":
		return cmdEvents(rest, stdout, stderr)
	default:
		outf(stderr, "fjbctl: unknown subcommand %q\n\n", cmd)
		printUsage(stderr)
		return 2
	}
}

func printUsage(w *os.File) {
	outln(w, `fjbctl — operator CLI for the fj-bellows control plane

Usage:
  fjbctl <subcommand> [flags]

Subcommands:
  health      Readiness snapshot (healthy, last tick, upstream probes).
  workers     List worker VMs. --watch redraws on each state-transition event.
  cache       Managed pull-through registry cache VM state.
  reconcile   Drive one synchronous reconcile tick; print the counter summary.
  events      Stream state-transition events as they happen.

Common flags (all subcommands):
  -listen <host:port>      Daemon control plane address.
                           Default 127.0.0.1:9876.
                           Override with $FJBCTL_LISTEN.
  -token-file <path>       Bearer token file for non-loopback deployments.
                           Override with $FJBCTL_TOKEN_FILE.
  -json                    Emit the proto-JSON response instead of the
                           human-readable rendering.

Examples:
  fjbctl health
  fjbctl workers --watch
  fjbctl reconcile
  fjbctl -listen 100.x.y.z:9876 -token-file ~/.fjb.token events`)
}
