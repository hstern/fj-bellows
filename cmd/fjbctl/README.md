# cmd/fjbctl

Operator-facing CLI for a running fj-bellows daemon. Speaks Connect/JSON
over HTTP to the control plane (`internal/control`) and renders the
responses in either a sorted human format (the default) or raw JSON.

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-server` | `http://127.0.0.1:9876` | daemon control plane URL |
| `-token-file` | `""` | bearer token file (mode 0600); required on non-loopback binds |
| `-json` | `false` | emit raw JSON instead of the default sorted text view |

The bearer token format mirrors the daemon's `-control-token-file` flag:
one trimmed, non-empty line per file.

## Commands

### `info`

Calls the daemon's `ProviderInfo` RPC (FJB-31) and renders the
provider-defined operator-debug key/value map. The text view emits the
provider slug as a comment line and the info map as `key: value` lines
sorted by key.

```sh
fjbctl info
# provider: linode
# account_balance_usd: -12.34
# cache_linode_id: 98765432
# capacity_full_count_24h: 0
# firewall_id: 4567890
# image: linode/debian13
# placement_group_id: 1234
# region: us-ord
# type: g6-standard-2
# vpc_id: 999
# workers_in_flight: 0

fjbctl -json info
# {
#   "provider": "linode",
#   "info": { ... }
# }
```

See the per-provider README (`internal/provider/<name>/README.md`) for
each provider's documented set of keys.
