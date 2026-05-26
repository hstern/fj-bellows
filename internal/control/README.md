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

Deferred to follow-up tickets: force-reap/force-provision,
pause/resume reconciler, config dump+reload, SSH-proxy, billing-window view,
provider-passthrough, the `fjbctl` companion CLI. v1 leans on
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
