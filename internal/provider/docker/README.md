# internal/provider/docker

A `provider.Provider` backed by local Docker containers, built on
[`github.com/docker/docker/client`](https://pkg.go.dev/github.com/docker/docker/client).

It provisions real containers running an sshd plus a `forgejo-runner`, giving
zero-cost real provisioning and SSH for development and integration tests with
no cloud account. Containers cost nothing, so it reports `BillingPerSecond` and
the core applies a plain idle timeout (no warm-hold).

The Docker calls are abstracted behind a small unexported interface
(`dockerAPI`), and TCP reachability behind a `dialer`, so the unit tests use
fakes and need no running Docker daemon.

## `provider_config` shape

```yaml
provider: docker
provider_config:
  image:             example/worker:latest  # required: worker image (sshd + forgejo-runner)
  ssh_port:          22                      # optional, in-container sshd port (default 22)
  ssh_ready_timeout: 30s                     # optional, wait for sshd to accept (default 30s)
```

`image` is the only required field; `Configure` errors if it is empty. The
client is built from the standard Docker environment (`DOCKER_HOST`, etc.) via
`client.FromEnv` with API-version negotiation.

## Lifecycle

- **Provision** — `ContainerCreate` + `ContainerStart` from `image`, stamping
  the pool tag as the container label `fj-bellows.tag=<spec.Tag>` and injecting
  the orchestrator key (see below). It inspects the container for its bridge
  IPv4 address, then polls TCP `ip:ssh_port` until sshd accepts or
  `ssh_ready_timeout` elapses. Returns
  `provider.Instance{ID: containerID, IPv4, CreatedAt, Tag}`.
- **Destroy** — `ContainerRemove` with `force=true`.
- **List(tag)** — `ContainerList` (including stopped containers) filtered by the
  daemon-side label filter `label=fj-bellows.tag=<tag>`.
- **BillingModel** — `BillingPerSecond`.

## Authorized-key injection contract

The orchestrator's SSH public key (`spec.AuthorizedKey`, an `authorized_keys`
line) is passed to the container in the `FJB_AUTHORIZED_KEY` environment
variable. The worker image's entrypoint **must**, before starting sshd, write
that value to the runner user's `~/.ssh/authorized_keys` (creating `~/.ssh`
with `0700` and the file with `0600`). When `spec.AuthorizedKey` is empty the
variable is omitted.

Minimal entrypoint sketch:

```sh
#!/bin/sh
if [ -n "$FJB_AUTHORIZED_KEY" ]; then
  mkdir -p /home/runner/.ssh
  printf '%s\n' "$FJB_AUTHORIZED_KEY" > /home/runner/.ssh/authorized_keys
  chmod 700 /home/runner/.ssh
  chmod 600 /home/runner/.ssh/authorized_keys
  chown -R runner:runner /home/runner/.ssh
fi
exec /usr/sbin/sshd -D
```

## Worker-image requirements

- Runs an **sshd** that listens on `ssh_port` (default 22) and accepts the
  orchestrator's key via the injection contract above.
- Honours `FJB_AUTHORIZED_KEY` in its entrypoint as described.
- Ships a **`forgejo-runner`** so the orchestrator can register and run jobs
  over SSH, the same as on a cloud worker.
- Is reachable on its container network IPv4 from wherever `fj-bellows` runs
  (e.g. on the same Docker bridge, or another container on a shared network).

cloud-init `UserData` is provider-agnostic and rendered by the core; this
provider does not consume it — the image entrypoint owns container setup.
