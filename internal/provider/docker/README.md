# internal/provider/docker

A `provider.Provider` backed by local Docker containers, plus a docker-exec
`orchestrator.Dispatcher`, built on
[`github.com/docker/docker/client`](https://pkg.go.dev/github.com/docker/docker/client).

It provisions real containers running a `forgejo-runner`, giving zero-cost real
provisioning for development and integration tests with no cloud account. Unlike
the SSH providers, the orchestrator reaches the worker over `docker exec` on the
local Docker socket: there is **no sshd, no SSH key, and no published port**.
Containers cost nothing, so it reports `BillingPerSecond` and the core applies a
plain idle timeout (no warm-hold).

The Docker calls are abstracted behind a small unexported interface
(`dockerAPI`), so the unit tests use a fake and need no running Docker daemon.

## `provider_config` shape

```yaml
provider: docker
provider_config:
  image:   example/worker:latest  # required: worker image (ships forgejo-runner)
  network: ci-net                 # optional: Docker network to attach the container to
```

`image` is the only required field; `Configure` errors if it is empty. The
client is built from the standard Docker environment (`DOCKER_HOST`, etc.) via
`client.FromEnv` with API-version negotiation.

A docker-only deployment needs no `ssh.private_key_file` in `config.yaml`; the
core validation relaxes that requirement for `provider: docker`.

## Lifecycle

- **Provision** — `ContainerCreate` + `ContainerStart` from `image`, stamping
  the pool tag as the container label `fj-bellows.tag=<spec.Tag>` and, if set,
  attaching the configured `network`. No key is injected and no port is exposed.
  Returns `provider.Instance{ID: containerID, IPv4: "", CreatedAt, Tag}` — the
  `IPv4` is unused because dispatch is via `docker exec`, not the network.
- **Destroy** — `ContainerRemove` with `force=true`.
- **List(tag)** — `ContainerList` (including stopped containers) filtered by the
  daemon-side label filter `label=fj-bellows.tag=<tag>`.
- **BillingModel** — `BillingPerSecond`.

## Dispatch (docker exec)

`ExecDispatcher` implements `orchestrator.Dispatcher` by driving the worker
container directly:

- **WaitReady** — polls `ContainerInspect` until the container's state is
  `Running`, bounded by a wait timeout and the context.
- **RunJob** — first writes the one-shot registration token to `/tmp/tok`
  inside the container by exec'ing `sh -c 'cat > /tmp/tok && chmod 600 /tmp/tok'`
  with the token on the exec's **STDIN** (so it never appears on any argv /
  process list), then exec's
  `forgejo-runner one-job --url <url> --uuid <uuid> --token-url file:/tmp/tok
  --label <labels> --handle <handle> --wait`, streaming to completion and
  failing on a nonzero exec exit code. A cancelled context closes the hijacked
  exec connection so an in-flight `one-job --wait` is aborted rather than leaked.

`ExecDispatcher` deliberately does **not** implement `orchestrator.HostKeyPinner`:
docker exec has no SSH host key, so the trust-on-first-use machinery does not
apply.

## Worker-image requirements

- Ships a **`forgejo-runner`** on `PATH` so the dispatcher can register and run
  jobs via `docker exec`.
- Provides a POSIX `sh` (used to write the token file).
- If jobs need Docker-in-job support (e.g. container-based steps), the image and
  its run environment must provide a usable Docker (e.g. a mounted Docker socket
  or Docker-in-Docker); this is a worker-image/runtime concern, not something
  the provider configures.

cloud-init `UserData` is provider-agnostic and rendered by the core; this
provider does not consume it — the image entrypoint owns container setup.
