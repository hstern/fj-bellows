package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
)

func cmdEvents(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
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

	// Streams live until the operator C-^c's the process or the daemon
	// shuts down; no per-request deadline.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	stream, err := client.StreamEvents(ctx, connect.NewRequest(&controlv1.StreamEventsRequest{}))
	if err != nil {
		return fmtErr(stderr, err)
	}
	defer func() { _ = stream.Close() }()

	for stream.Receive() {
		ev := stream.Msg()
		// Skip the protocol-level stream_opened sentinel; it's a Connect
		// quirk (forces response headers to flush on a quiet daemon) and
		// has no operator meaning.
		if ev.Type == "stream_opened" {
			continue
		}
		if cf.json {
			emitJSONEvent(stdout, ev)
		} else {
			emitHumanEvent(stdout, ev)
		}
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil {
		return fmtErr(stderr, err)
	}
	return 0
}

func emitHumanEvent(w io.Writer, ev *controlv1.StreamEventsResponse) {
	ts := "??:??:??"
	if ev.At != nil {
		ts = ev.At.AsTime().Local().Format("15:04:05")
	}
	keys := make([]string, 0, len(ev.Attrs))
	for k := range ev.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+ev.Attrs[k])
	}
	outf(w, "%s  %-20s  %s\n", ts, ev.Type, strings.Join(pairs, " "))
}

func emitJSONEvent(w io.Writer, ev *controlv1.StreamEventsResponse) {
	// One JSON object per line so `fjbctl events --json | jq -c` works.
	out := map[string]any{
		"type":  ev.Type,
		"attrs": ev.Attrs,
	}
	if ev.At != nil {
		out["at"] = ev.At.AsTime().UTC().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	outln(w, string(b))
}
