# internal/provider/docker

The local-Docker implementation of `provider.Provider`. It talks to the local
Docker daemon by shelling out to the `docker` CLI — it does **not** import any
`github.com/docker/...` Go package. The earlier SDK-based attempt (closed PR #14)
tripped govulncheck (`GO-2026-4887`, unfixable) and pulled a large dependency
tree; CLI shell-out (issue #15, Option A) avoids both.

The orchestrator host therefore needs the `docker` binary on `PATH` (or an
absolute path supplied via `docker_bin`).

`provider_config` shape:

```yaml
provider_config:
  image: example/worker:latest   # required; must ship forgejo-runner on PATH
  network: my-network            # optional; --network passed to docker run
  docker_bin: docker             # optional; default "docker"
  wait_timeout: 30s              # optional; bounds WaitReady polling
  volumes:                       # optional; one -v <host>:<container>[:mode] per entry
    - /var/run/docker.sock:/var/run/docker.sock
```

Mounting the host Docker socket via `volumes` is the standard way to let
`forgejo-runner` inside the worker spawn per-step containers on the host
daemon without nested Docker.

## Worker image contract

The image MUST:

- Contain `forgejo-runner` on `PATH` (the dispatcher invokes `forgejo-runner
  one-job ...`).
- Have a long-running entrypoint (e.g. `sleep infinity` or a supervisord-style
  process), so the container stays alive long enough for `docker exec` to land
  in it and run the job. There is no sshd, no SSH key, and no published port
  involved — the orchestrator does not connect to the container over the
  network.

## Lifecycle

- **Provision** — `docker run -d --label fj-bellows.tag=<tag> [--name <name>]
  [--network <net>] <image>`. The container ID returned on stdout becomes
  `Instance.ID`. `CreatedAt` is `time.Now().UTC()` (the docker daemon would
  return an equivalent value; using our clock avoids an extra `inspect`).
  `Instance.IPv4` is intentionally left empty: the dispatcher addresses the
  worker by container ID.
- **Destroy** — `docker rm -f <id>`.
- **List(tag)** — `docker ps -a --filter label=fj-bellows.tag=<tag>
  --format '{{.ID}}\t{{.Names}}\t{{.CreatedAt}}'`, parsed into
  `[]provider.Instance`. The tag label is the sole basis on which the
  orchestrator owns containers; multiple deployments on one host MUST use
  distinct `tag`s.
- **BillingModel** — `BillingPerSecond` (containers are local, there is no
  hourly rounding to amortize).

## Dispatch

A docker-exec dispatcher (`ExecDispatcher`) lives in this package and is
selected by the composition root when `provider: docker`. It implements
`orchestrator.Dispatcher` but deliberately does NOT implement `HostKeyPinner`
(that capability is SSH-specific).

- `WaitReady` polls `docker inspect -f '{{.State.Running}}' <id>` until the
  container reports `true`, bounded by `wait_timeout` / ctx.
- `RunJob` does two execs into the container:
  1. `docker exec -i <id> sh -c 'cat > /tmp/tok && chmod 600 /tmp/tok'`, with
     the registration token piped on stdin — never on argv, so it does not
     appear in the process list.
  2. `docker exec <id> forgejo-runner one-job --url <forgejoURL> --uuid <uuid>
     --token-url file:/tmp/tok --label <labels-csv> --handle <handle> --wait`.
  A cancelled context kills the exec and returns `ctx.Err()`.

## Testability

A small `cli` interface fronts the docker invocations from both the provider
and the dispatcher. Unit tests inject an in-memory fake; `go test` needs
neither the `docker` binary nor a running docker daemon.

## What about SSH config?

A docker-only deployment can omit `ssh.private_key_file` entirely — the
top-level config validator only requires it for non-docker providers. The
orchestrator passes an empty `AuthorizedKey` to cloud-init (which is itself
inert under this provider — no cloud-init runs in the container).
