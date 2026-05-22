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
./fj-bellows -config config.yaml
```

All secrets (Forgejo + provider tokens) live inline in `config.yaml`; the SSH
key is referenced by path. See [`config.example.yaml`](config.example.yaml).

### Container image

Multi-arch images (`linux/amd64`, `linux/arm64`) are published by CI to
`ghcr.io/hstern/fj-bellows` (and to Docker Hub when configured). Run with your
`config.yaml` mounted (and the SSH key it references):

```sh
docker run -d --name fj-bellows \
  -v /etc/fj-bellows:/etc/fj-bellows:ro \
  ghcr.io/hstern/fj-bellows:latest
```

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

## Running multiple deployments

fj-bellows decides which cloud instances it owns **solely by a tag** (`tag:` in
config, default `fj-bellows`): it only ever lists, adopts, or destroys instances
carrying that tag, so unrelated instances in the same cloud account are never
touched. But if you run more than one fj-bellows against the **same cloud
account**, you must give each a **distinct `tag`** — two deployments sharing a
tag will see each other's VMs and tear them down. The singleton lock only
prevents duplicate copies of one deployment on one host; it does not coordinate
across deployments. The daemon warns at startup if the default tag is in use.

## Security notes

- **config.yaml holds secrets** (Forgejo + provider tokens). Keep it `chmod 600`;
  the daemon warns on startup if it (or the SSH key) is readable by other users.
- **SSH host keys are verified.** For each worker the orchestrator generates a
  fresh ed25519 host key, injects the private half via cloud-init, and pre-pins
  the public half — so the SSH dispatch connection is verified on the very first
  dial, with no trust-on-first-use window for a man-in-the-middle to capture the
  one-shot token. (If no host key is seeded, the dispatcher falls back to
  per-VM trust-on-first-use pinning.)
- The worker VM never holds the Forgejo admin token; only a single-use ephemeral
  registration token, delivered over SSH via stdin (never the command line).
- CI runs `govulncheck` against the dependency tree.

## Status

Built milestone by milestone:

- **M1** — poll, provision one VM, run an ephemeral `one-job`, destroy on idle.
- **M2** — warm-hold + `:55` billing-hour teardown.
- **M3** — orphan sweep, three-source reconcile (jobs + runners + instances) with
  crash-recovery, graceful shutdown drain, scale-to-N, deterministic SSH
  host-key verification, and a provider-selectable dispatch mechanism.

A local Docker provider (containers as workers, dispatched via `docker exec`) is
in progress — see issue [#15](https://github.com/hstern/fj-bellows/issues/15).

## License

MIT — see [LICENSE](LICENSE).
