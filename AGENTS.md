# AGENTS.md

Guidance for AI agents (and humans) working in this repository.

## What this is

fj-bellows is a pluggable, ephemeral CI-runner autoscaler for Forgejo Actions.
It polls the Actions job queue, provisions cloud worker VMs, runs ephemeral
per-job runners on them, warm-holds for the paid billing hour, and tears them
down per the provider's billing model. See [README.md](README.md) and
[docs/design.md](docs/design.md).

## Build, test, lint

```sh
make build              # go build ./...
make race               # go test -race ./...  (orchestrator is concurrent — always -race)
make lint               # golangci-lint run ./...  (config in .golangci.yml)
make vuln               # govulncheck ./...
```

CI runs these in two jobs (`.github/workflows/ci.yml`): `test` (vet, build,
`-race` tests, govulncheck) and `lint` (golangci-lint). Both checks should be
required by branch protection before a PR can merge (configured on the GitHub
repo, not in-tree). `//nolint` directives must name the linter and give a reason
(enforced by `nolintlint`).

## Conventions (please keep)

- **Every package has a `README.md`.** Add one when you add a package.
- **Unit tests for everything.** New behavior comes with tests; keep them fast
  and hermetic (no real network/cloud — use the mocks).
- **Interfaces have hand-written mocks** under a sibling `mock/` package
  (func-field fakes, call recording, concurrency-safe). No codegen tool
  dependency. See `internal/provider/mock` and `internal/orchestrator/mock`.
- **No secrets or deployment specifics in the repo.** No real hostnames, account
  identifiers, usernames, tokens, regions tied to an account, or homelab
  details — in code, tests, docs, or examples. Use generic placeholders. CI
  image names derive from `github.repository` (ghcr) and a secret (Docker Hub),
  not a committed account string.
- Standard library `log/slog` for logging; no logging framework.

## Architecture invariants (don't break these)

- **Billing model is a provider attribute**, not hardcoded. A provider declares
  `BillingModel()`; the core picks the teardown policy
  (`internal/orchestrator/teardown.go`). Per-second → idle timeout; hourly
  round-up → warm-hold + the `:55` rule.
- **The reconcile loop is the single writer of provisioning decisions.**
  Dispatch and teardown goroutines mutate only their own node's state via the
  locked `Pool`. In-flight provisions count as `pending` so concurrent ticks
  don't over-provision.
- **Teardown timers are derived from `Instance.CreatedAt` each tick**, never
  stored, so they survive a restart. Crash recovery rebuilds the pool from
  `provider.List(tag)`.
- **The core never knows provider-specific config.** `provider_config` is an
  opaque `yaml.Node` decoded by the chosen provider (deferred decode).
- **Worker VMs never hold an admin token.** The orchestrator mints the ephemeral
  registration and delivers the one-shot token at dispatch time.
- **Dispatch is mechanism-agnostic and selected per provider.** The `Dispatcher`
  interface takes `(id, addr)`; SSH uses `addr`, a docker-exec dispatcher uses
  `id`. The composition root (`cmd/fj-bellows`) injects it. SSH host keys are
  verified via a per-VM key injected through cloud-init and pre-pinned (with TOFU
  fallback) — don't regress to ignoring host keys.
- **Workers reach Forgejo through the dispatch SSH session — *and so do the
  containers the runner spawns for each step*.** The SSH dispatcher opens a
  reverse port-forward on the dispatch connection and injects a `/etc/hosts`
  override on the worker so the runner process resolves the Forgejo hostname
  to `127.0.0.1`. The dispatcher also writes a forgejo-runner config with
  `container.network: host` and `container.options: --add-host=<host>:127.0.0.1`
  so every job container shares the worker's network namespace (reaching the
  tunnel listener via loopback) and gets the same hosts override. Without the
  runner config, `actions/checkout` and any other step that touches Forgejo
  from inside a containerized step NXDOMAINs even though the runner process
  is happily on the tunnel. Don't drop the runner config; don't reintroduce
  an out-of-process side-car tunnel.
- **The runner config also sets `container.docker_host: automount`** so the
  host docker socket is bind-mounted into every spawned job container. Without
  it, `docker …` steps fail with `no such file or directory`. The worker is
  single-tenant ephemeral and treats its job as trusted (operator's own
  repos), matching every other CI runner stack — same security posture as
  GitHub Actions, GitLab Runner, Drone.
- **The managed cache VM outlives the worker fleet — don't reap it on
  last Destroy.** The cache VM, its bucket, and the scoped key are
  intentionally NOT reaped from the per-instance `Linode.Destroy` path
  (FJB-12). Reaping on every idle-to-empty transition burned 3-5 min
  of cold-start cache boot on the next job and created a deadlock
  window where the next Provision needed the VPC IP from a just-
  reaped VM. Firewall/VPC reapers still fire correctly because the
  cache VM keeps them in use. The cache is still re-creatable on
  demand via `cache.ensure()` (FJB-10) for unrelated vanishings
  (Linode incident, manual delete). Don't add the cache reap back to
  `Linode.Destroy`.
- **Every managed Linode resource must be lazy-recreatable from Provision.**
  Firewall, placement group, and VPC are eager-created at Configure
  but reaped on last-Destroy when their attached-thing count drops to
  zero. The cache VM is eager-created and stays warm (see above). The reaper clears the cached ID to zero; the next
  Provision MUST re-create them via `ensure(ctx)` (a no-op when id !=
  0, otherwise calls `ensureAtConfigure`) before reading any IDs into
  `InstanceCreateOptions`. Without this, `PlacementGroup.ID = 0`
  reaches Linode and wedges the orchestrator in a 10s-retry loop with
  "Placement Group ID: 0 not found" — FJB-10. Don't add a new managed
  resource without the matching `ensure()` and an entry in
  `ensureManagedResources`.
- **The Linode managed-cache VM reaches upstream through a persistent
  orchestrator-side `ssh -R`, not its own NIC.** The cache VM is
  provisioned at Configure-time and never participates in a dispatch SSH
  session, so it can't inherit the per-dispatch worker tunnel — instead
  the linode provider starts a long-lived reconnect-loop goroutine that
  opens a reverse-forward listener on `127.0.0.1:<upstream-port>` on the
  cache VM and bridges each accepted connection to the upstream from the
  orchestrator's own network namespace. The cache cloud-init pins
  `127.0.0.1 <upstream-host>` in /etc/hosts so zot's sync extension
  hits the loopback listener; TLS SNI is unchanged, so a public-CA cert
  on the upstream still validates end-to-end. The orchestrator's SSH
  identity is plumbed into the linode provider via `SetSSHIdentity` from
  `cmd/fj-bellows` before `Configure` runs. Closes FJB-7. Don't drop the
  /etc/hosts override; don't try to give the cache VM direct LAN access
  via NAT (the tunnel is the answer).
- **The Linode managed firewall is owned by `cfg.Tag`, eager-created at
  Configure, refreshed on a goroutine, and reaped on last Destroy** — no
  Provider-level shutdown hook. The `firewall:` block is mutually exclusive
  with the simpler `firewall_id:` (attach-to-existing) mode. The managed
  **placement group** (`placement_group:`) follows the same lifecycle
  shape — eager Configure, reap on last Destroy, mutex with
  `placement_group_id`. PGs have no `tags` field, so ownership matching
  is by label alone. `allow_inbound` accepts CIDRs plus the `auto` sentinel; sentinel
  failure or empty resolution is FATAL at Configure (avoid silent wedge),
  but runtime refresh failure keeps the previous-known-good rules in place
  (don't punish a working deployment for a transient network blip).
- **Scale-to-N architecture; do not hardcode the single-VM assumption.**
  `scale.max` bounds it (default 1).
- **A deployment owns instances solely by `cfg.Tag`.** `provider.List(tag)` is
  the entire world the reconcile/orphan-sweep acts on. Multiple deployments on
  one cloud account MUST use distinct tags or they destroy each other's VMs; the
  daemon warns when the default tag (`config.DefaultTag`) is used.
- **The Dockerfile pre-creates `/run/fj-bellows.lock` owned `65532:65532`** so
  the distroless nonroot user can take the singleton flock without a writable
  `/run`. Don't move the default `-lock` flag value without updating the
  Dockerfile.

## Known limitations / open work

- **Local Docker provider is blocked on issue #15** — the Docker Go SDK trips
  govulncheck (GO-2026-4887, unfixable) and pulls a heavy dep tree. Draft in
  PR #14; decision (CLI shell-out / build-tag / suppress) pending.
- **Concurrent jobs per VM** (capacity-N daemon mode) is a deliberate non-goal
  for now — tracked in issue #13. Default stays ephemeral one-job-per-VM.

## Config & secrets

`config.yaml` holds the Forgejo and provider tokens **inline** (keep it
`chmod 600`). The SSH private key is referenced by **path**
(`ssh.private_key_file`), not inlined. See `config.example.yaml`.

## Adding a provider

1. New package `internal/provider/<name>` implementing `provider.Provider`.
2. `provider.Register("<name>", ...)` in its `init`.
3. Blank-import it in `cmd/fj-bellows/main.go`.
4. Decode your fields from the opaque node in `Configure`; report the correct
   `BillingModel()`.
5. Add a `README.md` and tests.
