# internal/control

Operator-facing control plane for the running fj-bellows daemon.

## What it serves

One TCP listener (default `127.0.0.1:9876`, override with `-control-listen`)
multiplexes three protocols on a single mux:

- **ConnectRPC** at `/<package>.<Service>/<Method>`, speaking Connect/JSON,
  gRPC, and gRPC-Web. The service is `fjbellows.control.v1.ControlService`
  (proto in `proto/`, generated code in `gen/`).
- **`/healthz`** — plain HTTP shim for k8s-style liveness/readiness probes and
  `curl --fail`. Returns 200 + tiny JSON when healthy, 503 otherwise.
- **`/metrics`** — Prometheus exposition (added in a later PR).

HTTP/2 cleartext (`UnencryptedHTTP2`) is enabled so gRPC clients work over the
loopback-bound socket without TLS.

## v1 scope

PR1 (this one) ships the server skeleton + the `Health` RPC + the `/healthz`
shim. Subsequent PRs widen the proto + handler with:

| PR | RPC / surface |
| --- | --- |
| PR2 | `ListWorkers` |
| PR3 | `GetCache` |
| PR4 | `Reconcile` (unary), `StreamEvents` (server-streaming) |
| PR5 | plain `/metrics` |
| FJB-25 | `StreamLogs` (server-streaming structured slog records) |
| FJB-26 | `ForceReap`, `ForceProvision` (admin verbs; gated by `-enable-control-writes`) |
| FJB-27 | `Pause`, `Resume` (reconciler-quiesce verbs; same gate) |
| FJB-28 | `GetConfig` (read-only redacted YAML dump), `ReloadConfig` (hot-swap a subset; gated by `-enable-control-writes`) |
| FJB-30 | `ListWorkers` billing-window fields (`paid_hour_end_at`, `reap_eligible_at`, `billing_model`) |

Deferred to follow-up tickets: SSH-proxy, provider-passthrough. v1 leans
on loopback-binding as the default auth boundary; the bearer-token
interceptor (FJB-33, below) is what binds a non-loopback deployment.

## Auth on non-loopback binds (FJB-33)

When `-control-listen` is loopback (`127.0.0.1`, `localhost`, `[::1]`), the
control plane assumes the network is the auth boundary and accepts every
request. The default `127.0.0.1:9876` deployment needs no further config.

When `-control-listen` is anything else (`0.0.0.0`, a private LAN address, a
tailscale IP, …), the daemon **refuses to start** without
`-control-token-file /path/to/token`. The file holds one non-empty line of
token (whitespace trimmed); mode `0600` is the recommended posture.

Connect RPCs then require the header on every request:

```
Authorization: Bearer <contents of token file>
```

`/healthz` and `/metrics` stay open regardless — Prom scrapers and k8s
liveness probes can't reasonably carry per-request bearer creds, and what
they expose isn't sensitive enough to gate.

Sample bind for a tailscale-exposed daemon:

```sh
openssl rand -hex 32 > /etc/fj-bellows/control.token
chmod 600 /etc/fj-bellows/control.token

fj-bellows \
  -config /etc/fj-bellows/config.yaml \
  -control-listen 100.x.y.z:9876 \
  -control-token-file /etc/fj-bellows/control.token
```

A client (e.g. `fjbctl` once it lands, FJB-32) reads the same file and
injects the header. Out of scope for this milestone: SIGHUP-driven token
rotation, per-RPC allowlists (mutating verbs gated, read-only open), mTLS
termination — that last one belongs behind a reverse proxy.

## Force verbs (FJB-26)

`ForceReap` and `ForceProvision` are operator-facing escape hatches for
production incidents. They are off by default; the daemon enables them only
when `-enable-control-writes` is set.

- `ForceReap(instance_id)` — destroys a worker immediately, bypassing
  billing policy. Any in-flight teardown state is overridden. Returns
  `CodeNotFound` when the instance is not in the pool, `CodeInternal` when
  `provider.Destroy` fails (the node is reverted to `idle` so the next
  teardown tick or another force-reap can retry), and `CodePermissionDenied`
  when `-enable-control-writes` is unset.
- `ForceProvision()` — spawns one extra worker, bypassing `scale.max` for
  this single tick. Returns the new instance ID synchronously; async
  readiness errors land later as `worker_reaped` events on the
  `StreamEvents` stream. Returns `CodePermissionDenied` when
  `-enable-control-writes` is unset.

Both verbs run from the reconcile goroutine (kicked through the same
single-writer select that drives `Reconcile`), so they cannot race a
concurrent tick.

Every force call emits a slog `Info` line carrying the caller identity
threaded from the handler:

```
force-reap requested id=100 caller="peer=10.0.0.5:54312 token"
force-provision requested caller="peer=127.0.0.1:54312"
```

The `caller` string is built from the Connect request's peer address plus
a `token` marker when the request carried an `Authorization: Bearer`
header (we don't decode the token — its presence is the signal). When
nothing was threaded, the value is `"loopback"`.

### Enabling the writes

Loopback bind, no token: just pass `-enable-control-writes`. The network
is the auth boundary; anyone who can reach `127.0.0.1:9876` already owns
the daemon.

```sh
fj-bellows -config /etc/fj-bellows/config.yaml -enable-control-writes
```

Non-loopback bind: `-enable-control-writes` requires `-control-token-file`
too (the same token file the bearer-token gate reads). The daemon refuses
to start otherwise — exposing mutating verbs unauthenticated to the
network is never the intent.

```sh
fj-bellows \
  -config /etc/fj-bellows/config.yaml \
  -control-listen 100.x.y.z:9876 \
  -control-token-file /etc/fj-bellows/control.token \
  -enable-control-writes
```

The bearer-token gate and the writes gate are independent: a non-loopback
deployment that wants read-only mirror access (Health, ListWorkers,
GetCache, Reconcile, StreamEvents) over tailscale can leave
`-enable-control-writes` off and still hand out the token.

## Pause / Resume (FJB-27)

`Pause` and `Resume` are operator verbs for quiescing the reconcile loop —
useful during maintenance windows, capacity-full debugging, or when attaching
a debugger. Both share the FJB-26 `-enable-control-writes` gate; the daemon
returns `CodePermissionDenied` when the flag is unset.

- `Pause()` — stops the reconcile loop's auto-tick. Subsequent ticker ticks
  become no-ops. In-flight dispatch / provision / teardown goroutines keep
  running until they complete on their own; only new work is paused.
- `Resume()` — re-arms the auto-tick. Idempotent.

Explicit `Reconcile`, `ForceReap`, and `ForceProvision` RPCs **still fire**
while paused — an operator asking for a tick gets one. This is the contract
that lets you pause for debugging and then drive a single observed tick
without flipping resume → pause again.

The freshness counters (`last_tick_at`, ...) only advance on real reconciles,
so a long-paused daemon will report `healthy=false` even though it's
deliberately quiet. The new `paused` field on `HealthResponse` is the
operator's signal that this is intentional rather than a stuck upstream:

```
{ "healthy": false, "paused": true, "lastTickAt": "2026-05-25T12:00:00Z", ... }
```

Both verbs are audit-logged with the caller identity threaded from the
handler (same convention as FJB-26 force verbs):

```
paused caller="peer=10.0.0.5:54312 token"
resumed caller="peer=127.0.0.1:54312"
```

A `reconciler_paused` / `reconciler_resumed` event is also published on the
`StreamEvents` stream on each real transition (idempotent re-pauses /
re-resumes are silent on both the log and the event stream).

## Wire format for ad-hoc / e2e clients

Connect's JSON protocol is one POST per method. The e2e harness and any
debugging operator can use plain `curl`:

```sh
curl -sS -X POST \
  -H 'content-type: application/json' \
  -d '{}' \
  http://127.0.0.1:9876/fjbellows.control.v1.ControlService/Health
```

The plain HTTP shims are even simpler:

```sh
curl http://127.0.0.1:9876/healthz
```

For the server-streaming RPCs (`StreamEvents`, `StreamLogs`), the Connect
protocol uses HTTP/1.1 chunked transfer-encoding so plain `curl` works:

```sh
curl -N -sS -X POST \
  -H 'content-type: application/json' \
  -d '{"history_lines": 50, "instance_id": "vm-1"}' \
  http://127.0.0.1:9876/fjbellows.control.v1.ControlService/StreamLogs
```

## StreamLogs (FJB-25)

`StreamLogs` is a server-streaming RPC that fans the daemon's structured
slog records out to operator clients. Implementation lives in the sibling
[`logbus/`](logbus/README.md) package: the daemon's `slog.Logger` is built
around a `logbus.Handler` wrapper, so every `log.Info(...)` / `log.Warn(...)`
the orchestrator emits reaches both stderr (the wrapped text handler) AND
the bus.

Request shape:

- `instance_id` (optional): only deliver records whose `attrs["id"]`
  matches. Empty means no filter on this dimension.
- `handle` (optional): only deliver records whose `attrs["handle"]`
  matches. Empty means no filter on this dimension.
- `history_lines` (optional): number of recently-buffered records to
  replay before live streaming. `0` (the default) replays 100 lines; the
  daemon caps the replay at the bus's ring-buffer capacity
  (`logbus.HistoryCapacity = 1000`). To opt out of replay entirely, send a
  negative value (clamped to 0 → no replay).

Stream shape:

1. **Sentinel** — first message has empty `level`/`message` and a `now`
   timestamp. Connect server-streaming only writes response headers on
   the first Send, so the sentinel makes the client's `Open` return
   immediately even on a quiet daemon. Clients should skip it (same
   convention as StreamEvents).
2. **History replay** — up to `history_lines` previously-buffered records
   in chronological order.
3. **Live** — records as the daemon emits them, until the client
   disconnects or the bus drops the subscriber for slow consumption (in
   which case the server returns `CodeResourceExhausted`).

Each `StreamLogsResponse` carries `at`, `level` (slog's String form:
`"DEBUG"` / `"INFO"` / `"WARN"` / `"ERROR"`), `message`, and an `attrs`
map.

## ListWorkers billing window (FJB-30)

Each `Worker` in `ListWorkers` carries three fields that surface the
teardown policy's view of the worker, so operators can debug warm-hold /
reap timing from the control plane instead of from log archaeology:

- `billing_model` — `"per_second"` or `"hourly_round_up"`, matching the
  provider's `BillingModel()`. Empty for the zero policy.
- `reap_eligible_at` — the earliest instant the policy would tear this
  worker down: `last_busy + idle_timeout` for per-second, the next
  `created + N*billing_hour - hour_margin` mark for hourly.
- `paid_hour_end_at` — the next paid-hour boundary
  (`reap_eligible_at + hour_margin`). Empty for per-second.

Values are computed from `orchestrator.TeardownPolicy.Timing(node, now)`,
which is the read-only sibling of `ShouldTeardown` — same math, no
decision. The Linode e2e uses `billing_hour: 60s, hour_margin: 10s` so
both timestamps populate within seconds of worker creation.

## GetConfig and ReloadConfig (FJB-28)

`GetConfig` is the operator-side answer to "what is the daemon actually
using?" It serialises the resolved live config (defaults filled in by
`config.applyDefaults`, not the raw file as written) as YAML, with secrets
replaced by `<redacted>`. Always allowed — the response carries no
credentials and no admin verbs.

Redaction rules (see `internal/config/redact.go`):

- `forgejo.token` → the marker. The field stays present so the operator
  can confirm "yes, a token is configured."
- Inside the opaque `provider_config` blob: any mapping key whose
  case-insensitive name matches one of `token`, `password`, `secret`,
  `key`, `api_key`, `access_key`, `secret_key` has its scalar value
  replaced. Matching is *exact*, not substring — `tokenizer` and
  `secretRecipe` are NOT redacted.
- `ssh.private_key_file` (the *path*) passes through unchanged. The file
  it points to is the secret; the path is operator config.
- Everything else (Forgejo URL, scope, labels, scale, poll, tag, SSH
  user/port, the rest of `provider_config`) passes through unchanged.

`ReloadConfig` re-reads `config.yaml` from disk, validates it, and hands
the hot-reloadable subset to the orchestrator. It is gated by
`-enable-control-writes`. Returns the list of changed dotted-key field
names (e.g. `["poll.interval", "scale.max"]`); an empty list means the
re-read parsed to the same values that were already live.

The hot-reloadable subset is exactly the fields the reconcile loop reads
off `o.cfg` on each tick:

| Field | What it controls |
| --- | --- |
| `scale.max` | warm pool ceiling |
| `forgejo.labels` | label set advertised to workers and used to match jobs |
| `poll.interval` | reconcile cadence; the ticker is re-created on change |
| `poll.idle_timeout` | per-second billing teardown timer |
| `poll.hour_margin` | hourly-rounding teardown (the `:55` rule) |
| `poll.billing_hour` | hourly-rounding cycle length |
| `runner_version` | the forgejo-runner version baked into the next cloud-init |
| `drain_on_shutdown` / `drain_timeout` / `destroy_on_exit` | shutdown behaviour |

Restart-required fields — `ReloadConfig` refuses with
`CodeFailedPrecondition` and lists the offending fields:

| Field | Why a restart is required |
| --- | --- |
| `provider` | the provider client is built once at startup |
| `provider_config` | re-running `provider.Configure` would re-allocate firewalls/PGs/VPCs |
| `forgejo.url` / `forgejo.token` / `forgejo.scope` | the Forgejo client wraps these at startup |
| `tag` | switching tag mid-flight would orphan every live VM the daemon owns |
| `ssh.*` | the SSH signer is loaded once at startup |
| billing model | derived from the provider's compile-time `BillingModel()` |

The reload is atomic: if any non-hot field has drifted, no hot field is
applied either. The operator's edit is rejected wholesale; they fix the
non-hot field (or accept that they need a restart) and try again. The
config-path returned by `GetConfig` makes that "fix and retry" loop
self-evident — the operator knows which file to edit.

When `poll.interval` changes, the orchestrator's `Run` goroutine
recreates the ticker via a one-slot signal channel so the new cadence
takes effect on the next tick boundary. This is the only field that
touches live state outside `o.cfg` — every other hot field is consumed
by the reconcile loop on the next read of `o.cfg`.

Each `ReloadConfig` call emits an audit log line with the caller
identity (same convention as the force verbs), so a deployment can trace
"who reloaded what, when" against the slog stream.

## Backend abstraction

The handler depends only on a small `Backend` interface (see `backend.go`).
`*orchestrator.Orchestrator` does not implement it directly — `cmd/fj-bellows`
injects a thin adapter (`controlBackend` in `main.go`) so this package owns
the wire types and the orchestrator stays free of generated-protobuf imports.

Hand-written fake `Backend` lives in `mock/` per the repo convention.

## Regenerating proto

```sh
make proto         # buf generate → gen/
make proto-check   # CI safety: regenerate and fail on drift
```

You need `buf`, `protoc-gen-go`, and `protoc-gen-connect-go` on `$PATH`.
Install with `brew install bufbuild/buf/buf` and
`go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
connectrpc.com/connect/cmd/protoc-gen-connect-go@latest`.
