package main

import (
	"context"
	"flag"
	"io"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
)

func cmdWorkers(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workers", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	registerCommonFlags(fs, &cf)
	watch := fs.Bool("watch", false, "redraw the worker table on every state-transition event")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	client, err := cf.client()
	if err != nil {
		return fmtErr(stderr, err)
	}

	if *watch {
		return runWatchWorkers(client, cf, stdout, stderr)
	}

	ctx, cancel := contextWithTimeout()
	defer cancel()
	resp, err := client.ListWorkers(ctx, connect.NewRequest(&controlv1.ListWorkersRequest{}))
	if err != nil {
		return fmtErr(stderr, err)
	}
	if cf.json {
		return printJSON(stdout, stderr, resp.Msg)
	}
	renderWorkers(stdout, resp.Msg.Workers)
	return 0
}

// runWatchWorkers subscribes to the daemon's StreamEvents and redraws the
// worker table on every state-transition event. Returns when the operator
// interrupts, the stream errors, or the daemon shuts down.
func runWatchWorkers(client interface {
	ListWorkers(context.Context, *connect.Request[controlv1.ListWorkersRequest]) (*connect.Response[controlv1.ListWorkersResponse], error)
	StreamEvents(context.Context, *connect.Request[controlv1.StreamEventsRequest]) (*connect.ServerStreamForClient[controlv1.StreamEventsResponse], error)
}, cf commonFlags, stdout, stderr io.Writer,
) int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	stream, err := client.StreamEvents(ctx, connect.NewRequest(&controlv1.StreamEventsRequest{}))
	if err != nil {
		return fmtErr(stderr, err)
	}
	defer func() { _ = stream.Close() }()

	// First message is the stream_opened sentinel — skip it, then render
	// the initial table from a fresh ListWorkers.
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return fmtErr(stderr, err)
		}
		return 0
	}
	if err := redrawWorkers(ctx, client, stdout, cf.json, stderr); err != nil {
		return fmtErr(stderr, err)
	}

	for stream.Receive() {
		if !redrawingEvent(stream.Msg().Type) {
			continue
		}
		if err := redrawWorkers(ctx, client, stdout, cf.json, stderr); err != nil {
			return fmtErr(stderr, err)
		}
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil {
		return fmtErr(stderr, err)
	}
	return 0
}

// redrawingEvent reports whether an event type should trigger a refresh of
// the worker table. Worker-* and job-* events both move pool state; the
// reconcile_tick captures syncPool adoptions/drops the per-worker events
// don't surface as cleanly.
func redrawingEvent(typ string) bool {
	return strings.HasPrefix(typ, "worker_") ||
		strings.HasPrefix(typ, "job_") ||
		typ == "reconcile_tick"
}

// redrawWorkers refetches the pool and emits its current rendering to
// stdout. In JSON mode every refresh is a fresh JSON document on its own
// line so a downstream `jq -c` pipeline can consume the stream.
func redrawWorkers(ctx context.Context, client interface {
	ListWorkers(context.Context, *connect.Request[controlv1.ListWorkersRequest]) (*connect.Response[controlv1.ListWorkersResponse], error)
}, stdout io.Writer, asJSON bool, stderr io.Writer,
) error {
	rpcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := client.ListWorkers(rpcCtx, connect.NewRequest(&controlv1.ListWorkersRequest{}))
	if err != nil {
		return err
	}
	if asJSON {
		// Discard the printJSON return code; failure prints to stderr and
		// the watch loop continues.
		printJSON(stdout, stderr, resp.Msg)
		return nil
	}
	outln(stdout, "──── workers @ "+time.Now().Format(time.TimeOnly)+" ────")
	renderWorkers(stdout, resp.Msg.Workers)
	return nil
}

// renderWorkers writes the worker table to w with tab-aligned columns.
func renderWorkers(w io.Writer, workers []*controlv1.Worker) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	outln(tw, "INSTANCE\tSTATE\tIP\tAGE\tLAST_BUSY\tJOB")
	for _, wk := range workers {
		outf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			wk.InstanceId,
			wk.State,
			emptyDash(wk.Ip),
			ageOrDash(wk.CreatedAt),
			ageOrDash(wk.LastBusy),
			emptyDash(wk.CurrentJob),
		)
	}
	_ = tw.Flush()
	if len(workers) == 0 {
		outln(w, "(no workers)")
	}
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ageOrDash(ts interface{ AsTime() time.Time }) string {
	if ts == nil {
		return "-"
	}
	t := ts.AsTime()
	if t.IsZero() {
		return "-"
	}
	return time.Since(t).Truncate(time.Second).String() + " ago"
}
