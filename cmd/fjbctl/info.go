package main

import (
	"flag"
	"io"
	"sort"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
)

// cmdInfo runs `fjbctl info`. It calls the daemon's ProviderInfo RPC
// (FJB-31) and renders the provider slug + the operator-debug key/value
// map in either a sorted human view (default) or proto-JSON (-json).
func cmdInfo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	registerCommonFlags(fs, &cf)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	client, err := cf.client()
	if err != nil {
		return fmtErr(stderr, err)
	}

	ctx, cancel := contextWithTimeout()
	defer cancel()
	resp, err := client.ProviderInfo(ctx, connect.NewRequest(&controlv1.ProviderInfoRequest{}))
	if err != nil {
		return fmtErr(stderr, err)
	}

	if cf.json {
		return printJSON(stdout, stderr, resp.Msg)
	}

	outf(stdout, "provider: %s\n", emptyDash(resp.Msg.Provider))
	if len(resp.Msg.Info) == 0 {
		outln(stdout, "  (no info keys; provider does not implement InfoProvider)")
		return 0
	}
	keys := make([]string, 0, len(resp.Msg.Info))
	for k := range resp.Msg.Info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		outf(stdout, "  %s = %s\n", k, resp.Msg.Info[k])
	}
	return 0
}
