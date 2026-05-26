package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
	"github.com/hstern/fj-bellows/gen/fjbellows/control/v1/controlv1connect"
)

// runInfo executes `fjbctl info`. It calls the daemon's ProviderInfo RPC
// (FJB-31) and renders the response in either a sorted human view (the
// default) or raw JSON (the parent's -json flag).
//
// The text view emits the provider slug as a comment line and the
// info map as `key: value` lines sorted by key — jq-friendly for ad-hoc
// pipelines, but easier on the operator than reading the proto JSON
// envelope by hand.
func runInfo(args []string, c commonFlags, client controlv1connect.ControlServiceClient, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("info: unexpected arguments: %v", fs.Args())
	}

	resp, err := client.ProviderInfo(context.Background(),
		connect.NewRequest(&controlv1.ProviderInfoRequest{}))
	if err != nil {
		return fmt.Errorf("ProviderInfo: %w", err)
	}

	if c.json {
		return emitJSON(stdout, resp.Msg)
	}
	return emitInfoText(stdout, resp.Msg)
}

// emitJSON marshals the protoresponse as JSON. We marshal through
// encoding/json on the proto-generated struct (not protojson) because
// fjbctl is an operator CLI — the lowercase Go field names + `omitempty`
// shape from encoding/json reads more naturally on a terminal than
// protojson's `provider`/`info` proto-camelCase. If a script wants the
// canonical protojson shape it can curl the RPC directly.
func emitJSON(stdout *os.File, msg *controlv1.ProviderInfoResponse) error {
	out := struct {
		Provider string            `json:"provider"`
		Info     map[string]string `json:"info"`
	}{
		Provider: msg.Provider,
		Info:     msg.Info,
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// emitInfoText renders the response as a sorted text block. Example:
//
//	# provider: linode
//	account_balance_usd: -12.34
//	cache_linode_id: 98765432
//	capacity_full_count_24h: 0
//	firewall_id: 4567890
//	image: linode/debian13
//	placement_group_id: 1234
//	region: us-ord
//	type: g6-standard-2
//	vpc_id: 999
//	workers_in_flight: 0
func emitInfoText(stdout *os.File, msg *controlv1.ProviderInfoResponse) error {
	if _, err := fmt.Fprintf(stdout, "# provider: %s\n", msg.Provider); err != nil {
		return err
	}
	keys := make([]string, 0, len(msg.Info))
	for k := range msg.Info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := fmt.Fprintf(stdout, "%s: %s\n", k, msg.Info[k]); err != nil {
			return err
		}
	}
	return nil
}
