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

Deferred to follow-up tickets: logs streaming, force-reap/force-provision,
pause/resume reconciler, config dump+reload, SSH-proxy, billing-window view,
provider-passthrough, the `fjbctl` companion CLI, and bearer-token auth for
non-loopback binds. v1 leans on loopback-binding as the auth boundary.

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
