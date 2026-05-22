# fj-bellows

A pluggable, ephemeral CI-runner autoscaler for [Forgejo Actions](https://forgejo.org/).

fj-bellows polls a Forgejo instance's Actions job queue. When a job is waiting
and the pool is under capacity, it provisions a cloud VM, runs **ephemeral
per-job** runners on it, keeps the VM **warm for the rest of the billing hour
already paid for**, and tears it down once idle. The teardown policy adapts to
each cloud's billing model.

Linode is the first provider; AWS/GCP/Azure can drop in behind the same
in-tree `Provider` interface.

## Why

Many clouds (Linode, Hetzner, older AWS) bill whole hours of instance existence
**rounded up**: once a job spins a VM, the entire hour is paid regardless. So:

- Keep the VM **warm** for that paid hour — jobs 2..N start instantly instead of
  paying a fresh ~30–60s boot each time.
- **Idle-kill near the hour boundary** (at `creation + N*hour - margin`; the
  default 5-minute margin gives the `:55` rule, so the DELETE finishes before
  the next hour bills).
- A **busy** job rolls into the next paid hour; you never pay an idle hour.

For **per-second** billing clouds, warm-holding is pointless, so fj-bellows uses
a plain idle timeout instead. The billing model is a **provider attribute**, not
a hardcoded assumption — that is what makes the tool correct across clouds, not
just cheap on one.

## How it works

- **Poll**: `GET /api/v1/<scope>/actions/runners/jobs` returns waiting jobs,
  their required labels, and a per-attempt handle.
- **Ephemeral per-job**: the orchestrator registers a one-shot runner
  (`POST .../actions/runners {"ephemeral":true}`) and runs `forgejo-runner
  one-job ... --wait` on the warm VM over SSH. Forgejo invalidates the
  credentials and removes the registration after the single job. The worker VM
  never holds an admin token.
- **Reconcile**: every tick the orchestrator reconciles three sources — waiting
  jobs, registered runners, and provider instances — into one internal view,
  modeling each node as a state machine
  (Provisioning → Idle → Busy → Draining → Removing).
- **Orphan sweep**: every instance is tagged; instances unknown to the
  orchestrator or idle past their paid hour are destroyed, so a crash or a
  failed DELETE never leaks a billed VM.
- **Singleton lock**: an advisory file lock ensures only one daemon makes
  provisioning decisions.

## Requirements

- Forgejo **≥ v15.0** (job-queue API) and **forgejo-runner > 12.5** (ephemeral
  `one-job`).
- A Forgejo admin token (to mint registration tokens).
- A cloud provider account and API token.
- An SSH keypair the orchestrator uses to dispatch jobs to worker VMs.

## Configuration

See [`config.example.yaml`](config.example.yaml). The `provider_config` subtree
is opaque to the core and decoded by the selected provider.

## Build & run

```sh
go build ./cmd/fj-bellows
LINODE_TOKEN=... ./fj-bellows -config config.yaml
```

### Container image

Multi-arch images (`linux/amd64`, `linux/arm64`) are published by CI. Run with
your config and secrets mounted, and the provider token supplied via the
environment variable named in `provider_config.token_env`.

## Repository layout

| Path | Purpose |
|------|---------|
| `cmd/fj-bellows` | daemon entrypoint, wiring, singleton lock |
| `internal/config` | YAML config with deferred `provider_config` decode |
| `internal/forgejo` | Forgejo Actions REST client |
| `internal/provider` | `Provider` interface + registry |
| `internal/provider/linode` | Linode implementation |
| `internal/bootstrap` | cloud-init worker bootstrap template |
| `internal/orchestrator` | pool, state machine, reconcile, teardown, dispatch |

## Security notes

- **config.yaml holds secrets** (Forgejo + provider tokens). Keep it `chmod 600`;
  the daemon warns on startup if it (or the SSH key) is readable by other users.
- **SSH host keys are not verified.** Worker VMs are created fresh per billing
  hour with no pre-known host key, so the dispatch connection trusts the host on
  connect. The channel is encrypted and the VM authenticates the orchestrator by
  the injected key, but a man-in-the-middle on the path to the worker's public IP
  could capture the one-shot ephemeral runner token. Hardening path: inject a
  known host key via cloud-init or pin per-VM on first connect (TOFU).
- The worker VM never holds the Forgejo admin token; only a single-use ephemeral
  registration token, delivered over SSH via stdin (never the command line).
- CI runs `govulncheck` against the dependency tree.

## Status

Built milestone by milestone:

- **M1** — poll, provision one VM, run an ephemeral `one-job`, destroy on idle.
- **M2** — warm-hold + `:55` billing-hour teardown.
- **M3** — orphan sweep, three-source reconcile with crash-recovery, scale-to-N.

## License

MIT — see [LICENSE](LICENSE).
