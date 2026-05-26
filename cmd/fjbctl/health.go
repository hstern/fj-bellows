package main

import (
	"flag"
	"io"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
)

func cmdHealth(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
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
	resp, err := client.Health(ctx, connect.NewRequest(&controlv1.HealthRequest{}))
	if err != nil {
		return fmtErr(stderr, err)
	}

	if cf.json {
		return printJSON(stdout, stderr, resp.Msg)
	}

	state := "DEGRADED"
	if resp.Msg.Healthy {
		state = "HEALTHY"
	}
	outln(stdout, state)
	outf(stdout, "  last_tick           %s\n", ageOrNever(resp.Msg.LastTickAt))
	outf(stdout, "  last_provider_list  %s\n", ageOrNever(resp.Msg.LastProviderListAt))
	outf(stdout, "  last_forgejo_poll   %s\n", ageOrNever(resp.Msg.LastForgejoPollAt))
	if !resp.Msg.Healthy {
		return 1
	}
	return 0
}

// ageOrNever renders an "X ago" for a non-zero proto timestamp, or "never"
// when the timestamp was omitted on the wire.
func ageOrNever(ts interface{ AsTime() time.Time }) string {
	if ts == nil {
		return "never"
	}
	t := ts.AsTime()
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Truncate(time.Second).String() + " ago"
}
