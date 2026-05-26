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
| FJB-29 | `ExecOnWorker` (one-shot debug exec over the orchestrator's SSH path) |

Deferred to follow-up tickets: pause/resume reconciler, config dump+reload,
SSH-proxy, billing-window view, provider-passthrough. v1 leans on
loopback-binding as the default auth boundary; the bearer-token interceptor
(FJB-33, below) is what binds a non-loopback deployment.

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

## ExecOnWorker (FJB-29)

`ExecOnWorker(instance_id, command)` runs a single shell command on the
named worker over the orchestrator's existing SSH dispatcher. The
orchestrator already holds every worker's host key and signer, so the
RPC needs no new credentials — it's a thin operator convenience for
"poke at this specific VM" without rediscovering its address + key
file.

- Gated by `-enable-control-writes`; an exec is a write-equivalent
  verb. `CodePermissionDenied` when the flag is unset.
- The command is `sh -c <command>` on the worker; `shellQuote` keeps
  attacker-influenced bytes from breaking out of the quoting. No
  interactive TTY.
- Command size is capped at 64 KiB; oversize requests are
  `CodeInvalidArgument`.
- Each output stream (stdout, stderr) is truncated to 1 MiB. The
  response carries `truncated_stdout` / `truncated_stderr` with the
  original byte count when truncation happened, so the operator can
  tell when output was clipped (default 0 means "not truncated").
- A non-zero remote exit is NOT an error — it lands in `exit_code` so
  the operator sees the same signal as a local shell.
- The orchestrator refuses to exec on a `provisioning` (SSH may not be
  up yet) or `removing` (Destroy in flight) worker —
  `CodeFailedPrecondition`. `idle` and `busy` are both fine; an exec on
  a busy worker is an out-of-band debug poke and does not interfere
  with the dispatch session.
- Unknown instance → `CodeNotFound`.
- SSH dial / transport failures → `CodeInternal`.
- The docker provider has no SSH path; calling `ExecOnWorker` against
  it returns `CodeUnimplemented` (a docker-exec variant is a separate
  future RPC, not handled here — sorry).
- Every call emits an `Info` audit line carrying the caller identity
  threaded from the handler:

```
exec-on-worker requested id=100 caller="peer=10.0.0.5:54312 token"
```

The session is bound by the caller's context deadline; if none is
set, the daemon imposes a 60-second default so a hung remote command
can't pin the dispatch goroutine forever.

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
