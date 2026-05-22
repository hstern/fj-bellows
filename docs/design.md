# fj-bellows design

## Goal

Run Forgejo Actions jobs on **ephemeral, per-job** cloud runners, while paying
for cloud VMs efficiently across providers with different billing models. Linode
is the first provider; the same in-tree `Provider` interface accommodates
AWS/GCP/Azure later.

## Core idea: warm pool sized to the paid hour

Some clouds (Linode, Hetzner, older AWS) bill whole hours of instance existence
**rounded up**. Once a job spins up a VM, the whole hour is paid regardless. So:

- Keep the VM **warm** for that hour; jobs 2..N start instantly instead of
  paying a fresh ~30–60s boot each time.
- **Idle-kill near the hour boundary** at `created + N*hour - margin` (default
  margin 5m → the `:55` rule), so the DELETE finishes before the next hour
  bills.
- A **busy** job rolls into the next paid hour; never pay an idle hour.

For **per-second** billing clouds, warm-holding is pointless, so the policy is a
plain idle timeout. The billing model is a **provider attribute**, which is what
makes the tool correct cross-cloud, not merely cheap on Linode.

## Components

| Package | Responsibility |
|---------|---------------|
| `internal/config` | YAML load; deferred `provider_config` decode; inline tokens |
| `internal/forgejo` | poll waiting jobs; mint ephemeral runner registrations |
| `internal/provider` | `Provider` interface + name registry |
| `internal/provider/linode` | Linode implementation (hourly round-up) |
| `internal/bootstrap` | provider-agnostic cloud-init (embedded template) |
| `internal/orchestrator` | pool, state machine, reconcile, teardown, SSH dispatch |
| `cmd/fj-bellows` | wiring, flags, singleton lock, signal handling |

## Provider interface

```go
type Provider interface {
    Configure(node yaml.Node) error
    Provision(ctx, spec) (Instance, error)
    Destroy(ctx, id) error
    List(ctx, tag) ([]Instance, error)   // reconcile + orphan sweep
    BillingModel() BillingModel
}
```

The core hands each provider the opaque `provider_config` node and never
references provider-specific fields. `Instance.CreatedAt` comes from the
provider's clock and anchors the billing-hour timer.

## Node lifecycle

```
Provisioning -> Idle -> Busy -> Idle -> Draining -> Removing
```

Ephemeral registration auto-removes the Forgejo runner when `one-job` exits, so
there is no separate deregistration step the orchestrator must drive.

## Reconcile loop (each tick)

1. `provider.List(tag)` is ground truth. Adopt unknown instances (crash
   recovery — billing timers rebuilt from `CreatedAt`); drop vanished ones
   (Provisioning nodes are exempt; a new VM may not be listed yet). This is also
   the **orphan sweep**: a leaked/idle-past-hour tagged VM gets reclaimed.
2. Poll waiting jobs; keep those whose required labels this pool offers.
3. Dispatch serviceable jobs to Idle nodes; provision for the remainder, capped
   by `scale.max` (in-flight provisions counted as `pending`).
4. Apply the billing-model teardown policy to Idle nodes.

The reconcile loop is the **single writer of provisioning decisions**. A process
**singleton flock** ensures only one daemon runs.

## Job dispatch

The orchestrator holds the admin token; the worker VM never does. Per job it
registers an ephemeral runner, then over SSH (in-process `x/crypto/ssh`) writes
the one-shot token via stdin and runs:

```
forgejo-runner one-job --url <inst> --uuid <uuid> --token-url file:/tmp/tok \
  --label <labels> --handle <handle> --wait
```

`one-job` is the only ephemeral-capable command. Within-hour VM reuse weakens
job→job isolation, which is acceptable for trusted single-tenant CI.

## Worker bootstrap

cloud-init installs Docker and the `forgejo-runner` binary (arch detected via
`uname -m`, so amd64 and arm64 both work) and touches a readiness sentinel. It
carries no credentials.

## Milestones

- **M1** — poll, provision one VM, ephemeral `one-job`, destroy on idle.
- **M2** — warm-hold + `:55` billing-hour teardown.
- **M3** — orphan sweep, three-source reconcile (jobs + runners + instances)
  with crash recovery, graceful shutdown drain, scale-to-N.

## Prior art

`rustunit/gitea-ci-autoscaler` (billing-hour teardown + reconcile, but
Gitea/Hetzner/K3s-coupled, not ephemeral); AWS EC2 ASG
`ClosestToNextInstanceHour`; GitLab `IdleCount`/`IdleTime`. The
Forgejo + no-k8s + ephemeral + cross-cloud-billing combination is the gap this
fills.
