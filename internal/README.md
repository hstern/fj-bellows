# internal

All of fj-bellows' implementation lives here (nothing is a stable public API —
the only entrypoint is `cmd/fj-bellows`). Each package has its own README; this
is the map of how they fit together.

## Packages

| Package | Responsibility |
|---------|---------------|
| [`config`](config) | Load/validate YAML; keep `provider_config` as an opaque `yaml.Node` (deferred decode); inline secrets. |
| [`forgejo`](forgejo) | REST client: poll waiting jobs, mint ephemeral runner registrations. |
| [`provider`](provider) | The `Provider` interface + name registry, and `BillingModel`. |
| [`provider/linode`](provider/linode) | Linode implementation (hourly-round-up billing). |
| [`provider/mock`](provider/mock) | Test double for `Provider`. |
| [`bootstrap`](bootstrap) | Provider-agnostic cloud-init for workers (embedded; amd64/arm64). |
| [`orchestrator`](orchestrator) | Pool + node state machine, reconcile loop, billing-aware teardown, SSH dispatch. |
| [`orchestrator/mock`](orchestrator/mock) | Test doubles for the orchestrator's `JobSource` and `Dispatcher`. |

## How they fit

`cmd/fj-bellows` wires the pieces and constructs the orchestrator with three
collaborators behind interfaces — a `provider.Provider`, a `JobSource`
(`*forgejo.Client`), and a `Dispatcher` (SSH) — then runs the reconcile loop.

Each tick the orchestrator:

1. lists provider instances (`provider.List`) — ground truth; adopts unknown
   ones and reaps vanished/orphaned ones;
2. polls `forgejo.WaitingJobs` and filters to serviceable labels;
3. dispatches a job to an idle node, or provisions a new VM (rendering
   `bootstrap` cloud-init) bounded by `scale.max`;
4. for a dispatch, mints an ephemeral registration (`forgejo.RegisterEphemeral`)
   and runs `forgejo-runner one-job` on the VM over SSH;
5. applies the teardown policy chosen from the provider's `BillingModel`.

```
config ──▶ cmd/fj-bellows ──▶ orchestrator ──┬─▶ provider (linode)
                                              ├─▶ forgejo  (jobs + registrations)
                                              ├─▶ bootstrap (cloud-init)
                                              └─▶ Dispatcher (SSH one-job)
```

## Conventions

The interface/mock split exists so the orchestrator is unit-testable without a
real cloud or SSH. See [`AGENTS.md`](../AGENTS.md) for build/test/lint commands
and the architecture invariants that must hold across these packages.
