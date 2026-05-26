# cmd/fjbctl

Operator-facing CLI for the fj-bellows control plane. Speaks the
ConnectRPC service defined in `proto/fjbellows/control/v1` against a
running daemon's `-control-listen` address.

## Install

```sh
go install github.com/hstern/fj-bellows/cmd/fjbctl@latest
```

## Subcommands

| Subcommand | RPC | Description |
| --- | --- | --- |
| `fjbctl health` | `Health` | Readiness snapshot (healthy + last-tick / last-provider-list / last-forgejo-poll ages). Exits 0 if healthy, 1 otherwise. |
| `fjbctl workers` | `ListWorkers` | Table of every worker the orchestrator currently tracks (state, IP, age, last_busy, current_job, billing model, reap eta). Pass `--watch` to subscribe to `StreamEvents` and redraw on every state-transition event. |
| `fjbctl cache` | `GetCache` | Managed pull-through registry cache VM state — present/absent, Linode VM status, VPC IP, bucket region+label. |
| `fjbctl reconcile` | `Reconcile` | Drive one synchronous reconcile tick. Prints the per-tick summary (provisioned / dispatched / reaped / adopted / dropped + any errors). Exits 1 if the response includes errors. |
| `fjbctl events` | `StreamEvents` | Stream state-transition events (`worker_provisioned`, `job_complete`, `reconcile_tick`, …) until interrupted. The protocol-level `stream_opened` sentinel is skipped. |
| `fjbctl info` | `ProviderInfo` | Provider's operator-debug key/value map (Linode managed-resource IDs, capacity-full counter, account balance, region/type/image; docker reports daemon socket/image/network/wait_timeout). |

## Common flags

| Flag | Env var | Default | Notes |
| --- | --- | --- | --- |
| `-listen` | `FJBCTL_LISTEN` | `127.0.0.1:9876` | Either `host:port` or a full URL (`http://…`, `https://…`). |
| `-token-file` | `FJBCTL_TOKEN_FILE` | _unset_ | Bearer-token file required when the daemon binds non-loopback (FJB-33). The file is trimmed; whitespace-only is an error. |
| `-json` | — | `false` | Emit the raw proto-JSON response instead of the human-readable rendering. Streaming subcommands emit one JSON document per line. |

## Examples

```sh
# Local default (loopback bind, no auth).
fjbctl health
fjbctl workers
fjbctl cache
fjbctl reconcile
fjbctl info
fjbctl events            # Ctrl-C to exit.

# Remote daemon (tailscale, mTLS-terminated by a reverse proxy, …).
export FJBCTL_LISTEN=100.x.y.z:9876
export FJBCTL_TOKEN_FILE=~/.config/fjbctl/token
fjbctl health
fjbctl workers --watch

# Pipe-friendly machine output.
fjbctl events --json | jq -c 'select(.type == "job_complete")'
fjbctl info --json | jq '.info.capacity_full_count_24h'
```

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Success (and, for `health`, healthy). |
| `1` | RPC error, or `health` reported unhealthy, or `reconcile` returned errors. |
| `2` | Usage error (unknown subcommand, bad flag). |
