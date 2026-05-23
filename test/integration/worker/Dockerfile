# Worker image for the fj-bellows integration CI job.
#
# The orchestrator addresses workers by container ID and drives them via
# `docker exec`, so the image only needs:
#   - forgejo-runner on PATH (the dispatcher invokes `forgejo-runner one-job`).
#   - a long-running entrypoint so `docker exec` can land in the container.
#   - curl + ca-certificates for the runner's HTTP traffic to Forgejo.
#   - the docker CLI, in case a future workflow step needs it (self-sufficient).
#
# Versions are pinned so the required CI gate is deterministic; bump in one
# place and rebuild.

FROM debian:bookworm-slim

ARG FORGEJO_RUNNER_VERSION=12.10.1
ARG TARGETARCH=amd64

# - ca-certificates: TLS to Forgejo
# - curl: download the runner binary; available for workflow steps
# - docker-cli: a workflow step that runs `docker ...` finds the CLI on PATH
# - tini: PID 1 that reaps zombies cleanly (workflows spawn subprocesses)
# Pinned to bookworm-slim so the package set is stable across rebuilds.
RUN set -eu; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        docker.io \
        tini; \
    rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    arch="${TARGETARCH:-amd64}"; \
    url="https://code.forgejo.org/forgejo/runner/releases/download/v${FORGEJO_RUNNER_VERSION}/forgejo-runner-${FORGEJO_RUNNER_VERSION}-linux-${arch}"; \
    curl -fsSL -o /usr/local/bin/forgejo-runner.bin "$url"; \
    chmod 0755 /usr/local/bin/forgejo-runner.bin; \
    /usr/local/bin/forgejo-runner.bin --version

# The worker container has no Docker daemon, so forgejo-runner is configured for
# host mode (`docker_host: "-"`) — steps run directly inside this container as
# host processes instead of spawning a per-step container. The wrapper at
# /usr/local/bin/forgejo-runner injects --config so the orchestrator's
# `docker exec ... forgejo-runner one-job ...` command stays mechanism-agnostic.
RUN set -eu; \
    mkdir -p /etc/forgejo-runner; \
    printf '%s\n' \
      'log:' \
      '  job_level: info' \
      'container:' \
      '  docker_host: "-"' \
      > /etc/forgejo-runner/config.yaml; \
    printf '%s\n' \
      '#!/bin/sh' \
      'exec /usr/local/bin/forgejo-runner.bin --config /etc/forgejo-runner/config.yaml "$@"' \
      > /usr/local/bin/forgejo-runner; \
    chmod 0755 /usr/local/bin/forgejo-runner

# tini reaps zombies; sleep infinity keeps the container alive so the
# orchestrator can `docker exec` into it.
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["sleep", "infinity"]
