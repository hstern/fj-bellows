# test/integration

End-to-end integration test that exercises fj-bellows against a real Forgejo,
using the local Docker provider for workers. Runs in CI as the `integration`
job (`.github/workflows/ci.yml`) and is intended to become a required check
on `main`.

## What it does

1. Builds a worker image (`test/integration/worker/Dockerfile`) that ships
   `forgejo-runner` v12.10.1 plus `tini`, the `docker` CLI, `curl`, and
   `ca-certificates`. Entrypoint is `tini -- sleep infinity` so the
   orchestrator can `docker exec` into a long-lived container.
2. Brings up Forgejo (pinned tag) as a service container, with
   `FORGEJO__security__INSTALL_LOCK=true` set so it starts already installed
   and `forgejo admin user create` works immediately.
3. Runs `seed.sh`, which:
   - waits for `/api/v1/version` to be 200 (bounded, 120s);
   - creates an admin user via `docker exec ... forgejo admin user create`;
   - mints an API token via the REST API;
   - creates an organization and a repo (with `auto_init`);
   - commits `.forgejo/workflows/echo.yml` (workflow with `runs-on: docker`)
     via the contents API — the push auto-queues a workflow run;
   - polls `/actions/runners/jobs?labels=docker` until the job appears.
4. Builds `fj-bellows`, writes a `config.yaml` pointing at the service Forgejo
   with `provider: docker`, and launches `fj-bellows` in the background
   capturing stderr to `fj-bellows.log`.
5. Runs `assert.sh`, which validates:
   - the orchestrator did NOT log "poll waiting jobs" decode errors (i.e. the
     types in `internal/forgejo/types.go` match the live API);
   - the orchestrator acted on the queued job (a `provisioned` / `provision`
     / `register ephemeral runner` line is present);
   - a worker container with label `fj-bellows.tag=<tag>` was created.
6. Always (`if: always()`) cleans up containers, networks, and the orchestrator
   process; uploads `fj-bellows.log` and the Forgejo container log as run
   artifacts so future flakes are debuggable.

## What it deliberately does NOT assert (yet)

`forgejo-runner one-job --handle` and the REST ephemeral-registration
endpoint (`POST /actions/runners` returning `{uuid, token}`) are documented
as **Forgejo 15+** features (see `forgejo-runner one-job --help` and the
"This Forgejo instance does not support ephemeral runners; requires Forgejo
15 or newer" error returned by `forgejo-runner register --ephemeral` against
12.x). No released Forgejo provides them today, so:

- The integration test does NOT poll the workflow run for completion — the
  orchestrator's `RegisterEphemeral` call returns a "requires Forgejo >= 15"
  error against current Forgejo, which is expected.
- This validates the largest reliability-critical piece — that the live
  `/actions/runners/jobs` response decodes against `types.go` — without
  asserting on behavior that no released Forgejo supports yet.
- When Forgejo 15 ships, the assert script should be extended to wait for
  the workflow run to reach `success` and to remove the
  "FORGEJO 15+ required" caveat below.

## Local reproduction

```sh
# 1. Build the worker image.
docker build -t fj-bellows-worker:test test/integration/worker

# 2. Bring up Forgejo on localhost:3001.
docker run -d --name fjb-integ-forgejo \
  -e FORGEJO__security__INSTALL_LOCK=true \
  -e FORGEJO__server__ROOT_URL=http://localhost:3001/ \
  -e FORGEJO__server__HTTP_PORT=3000 \
  -e FORGEJO__server__DOMAIN=localhost \
  -e FORGEJO__actions__ENABLED=true \
  -p 3001:3000 \
  codeberg.org/forgejo/forgejo:12

# 3. Seed it.
export FORGEJO_URL=http://localhost:3001
export FORGEJO_CONTAINER=fjb-integ-forgejo
export FORGEJO_ADMIN_USER=bellows
export FORGEJO_ADMIN_PASS=adminpass1
export FORGEJO_ADMIN_EMAIL=admin@example.com
export FORGEJO_ORG=bellowsorg
export FORGEJO_REPO=demo
export FORGEJO_LABEL=docker
TOKEN=$(bash test/integration/seed.sh)

# 4. Build the daemon and write a config.
go build -o fj-bellows ./cmd/fj-bellows
# In CI we discover the docker network shared with the Forgejo service; for
# local reproduction the bridge network works fine.
FORGEJO_INTERNAL_URL=http://$(docker inspect fjb-integ-forgejo \
  --format '{{.NetworkSettings.IPAddress}}'):3000
cat > /tmp/fjb-integ-config.yaml <<YAML
forgejo:
  url: $FORGEJO_INTERNAL_URL
  token: $TOKEN
  scope: orgs/$FORGEJO_ORG
  labels: [$FORGEJO_LABEL]
tag: fjb-integ-local
scale:
  max: 1
provider: docker
provider_config:
  image: fj-bellows-worker:test
poll:
  interval: 2s
  idle_timeout: 5s
  hour_margin: 5m
YAML

# 5. Run fj-bellows for a while, then assert.
chmod 600 /tmp/fjb-integ-config.yaml
./fj-bellows -config /tmp/fjb-integ-config.yaml \
  -lock /tmp/fjb-integ.lock \
  -destroy-on-exit \
  -drain=false 2> /tmp/fjb-integ.log &
FJBELLOWS_PID=$!
sleep 30

FJBELLOWS_LOG=/tmp/fjb-integ.log FJBELLOWS_TAG=fjb-integ-local \
  bash test/integration/assert.sh

# 6. Cleanup.
kill "$FJBELLOWS_PID" 2>/dev/null || true
docker rm -f fjb-integ-forgejo || true
docker ps -a --filter label=fj-bellows.tag=fjb-integ-local -q | \
  xargs -r docker rm -f
```
