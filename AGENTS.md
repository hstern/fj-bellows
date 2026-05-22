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
  details — in code, tests, docs, or examples. Use generic placeholders. The
  Docker image name in CI comes from a secret, not a committed string.
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
  registration and delivers the one-shot token over SSH at dispatch time.
- **Scale-to-N architecture; do not hardcode the single-VM assumption.**
  `scale.max` bounds it (default 1).

## Known limitations

- **SSH host keys are not verified** (`internal/orchestrator/dispatch.go`,
  `hostKeyCallback`). Fresh per-hour VMs have no pre-known host key; the dispatch
  connection is exposed to a MITM that could capture the one-shot ephemeral
  token. Hardening path: inject a known host key via cloud-init, or per-VM TOFU
  pinning. Don't "fix" this by silently changing the policy — it's a deliberate,
  documented trade-off.

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
