package main

import (
	"flag"
	"io"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
)

func cmdReconcile(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
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
	resp, err := client.Reconcile(ctx, connect.NewRequest(&controlv1.ReconcileRequest{}))
	if err != nil {
		return fmtErr(stderr, err)
	}

	if cf.json {
		return printJSON(stdout, stderr, resp.Msg)
	}

	m := resp.Msg
	outln(stdout, "reconcile complete")
	outf(stdout, "  provisioned  %d\n", m.Provisioned)
	outf(stdout, "  dispatched   %d\n", m.Dispatched)
	outf(stdout, "  reaped       %d\n", m.Reaped)
	outf(stdout, "  adopted      %d\n", m.Adopted)
	outf(stdout, "  dropped      %d\n", m.Dropped)
	if len(m.Errors) > 0 {
		outln(stdout, "  errors:")
		for _, e := range m.Errors {
			outf(stdout, "    - %s\n", e)
		}
		return 1
	}
	return 0
}
