package main

import (
	"flag"
	"io"

	"connectrpc.com/connect"

	controlv1 "github.com/hstern/fj-bellows/gen/fjbellows/control/v1"
)

func cmdCache(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache", flag.ContinueOnError)
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
	resp, err := client.GetCache(ctx, connect.NewRequest(&controlv1.GetCacheRequest{}))
	if err != nil {
		return fmtErr(stderr, err)
	}

	if cf.json {
		return printJSON(stdout, stderr, resp.Msg)
	}

	m := resp.Msg
	if !m.Present {
		outln(stdout, "cache: not configured")
		return 0
	}
	outln(stdout, "cache: present")
	outf(stdout, "  linode_id         %d\n", m.LinodeId)
	outf(stdout, "  vm_state          %s\n", emptyDash(m.VmState))
	outf(stdout, "  vpc_ip            %s\n", emptyDash(m.VpcIp))
	outf(stdout, "  bucket            %s/%s\n", emptyDash(m.BucketRegion), emptyDash(m.BucketLabel))
	outf(stdout, "  adopted_existing  %t\n", m.AdoptedExisting)
	return 0
}
