#!/usr/bin/env bash
# Seed a freshly-started Forgejo with:
#   - an admin user (the install-lock env var is set in the workflow so the
#     container starts directly into the "installed" state and we can use
#     `forgejo admin user create` without going through the web wizard);
#   - an API token (printed to stdout);
#   - an organization;
#   - a repository in that organization with Actions enabled;
#   - a .forgejo/workflows/echo.yml workflow whose `runs-on` matches the
#     pool labels, committed via the contents API (which triggers a workflow
#     run automatically).
#
# Required env:
#   FORGEJO_URL          base URL (e.g. http://localhost:3000)
#   FORGEJO_CONTAINER    name of the running Forgejo container (for `docker exec`)
#   FORGEJO_ADMIN_USER   admin username to create
#   FORGEJO_ADMIN_PASS   admin password
#   FORGEJO_ADMIN_EMAIL  admin email
#   FORGEJO_ORG          org to create
#   FORGEJO_REPO         repo name to create under the org
#   FORGEJO_LABEL        single runs-on label (e.g. "docker")
#
# Optional env:
#   FORGEJO_WORKFLOW_CONTAINER_OPTS  if set, the seeded workflow runs its job
#                                    inside an alpine container with these
#                                    `container.options` (e.g. "--network host"
#                                    so a reverse-tunnel-on-loopback design
#                                    works for steps too). Unset = no container
#                                    block, default forgejo-runner behavior.
#
# Prints the admin token on stdout. Logs go to stderr.

set -euo pipefail

log() { printf '[seed] %s\n' "$*" >&2; }

: "${FORGEJO_URL:?}"
: "${FORGEJO_CONTAINER:?}"
: "${FORGEJO_ADMIN_USER:?}"
: "${FORGEJO_ADMIN_PASS:?}"
: "${FORGEJO_ADMIN_EMAIL:?}"
: "${FORGEJO_ORG:?}"
: "${FORGEJO_REPO:?}"
: "${FORGEJO_LABEL:?}"

# Wait for Forgejo to answer /api/v1/version (bounded; never infinite).
log "waiting for $FORGEJO_URL to be ready"
ready=0
for i in $(seq 1 120); do
  code=$(curl -fsS -o /dev/null -w '%{http_code}' "$FORGEJO_URL/api/v1/version" 2>/dev/null || true)
  if [ "$code" = "200" ]; then
    log "Forgejo ready after ${i}s"
    ready=1
    break
  fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  log "ERROR: Forgejo did not become ready within 120s"
  exit 1
fi

# Create admin user. Idempotent: tolerate "user already exists" on reruns.
log "creating admin user $FORGEJO_ADMIN_USER"
if ! docker exec --user git "$FORGEJO_CONTAINER" \
      forgejo admin user create \
        --username "$FORGEJO_ADMIN_USER" \
        --password "$FORGEJO_ADMIN_PASS" \
        --email "$FORGEJO_ADMIN_EMAIL" \
        --admin \
        --must-change-password=false >&2; then
  log "admin user create returned non-zero (likely already exists); continuing"
fi

# Mint an API token. Tokens have unique names; suffix with the run id so reruns
# in the same Forgejo instance do not collide.
token_name="integ-$(date +%s%N)-$$"
log "minting API token $token_name"
token_response=$(
  curl -fsS -u "$FORGEJO_ADMIN_USER:$FORGEJO_ADMIN_PASS" \
    -H 'Content-Type: application/json' \
    -X POST \
    -d "{\"name\":\"$token_name\",\"scopes\":[\"write:admin\",\"write:repository\",\"write:organization\",\"write:user\"]}" \
    "$FORGEJO_URL/api/v1/users/$FORGEJO_ADMIN_USER/tokens"
)
api_token=$(printf '%s' "$token_response" | jq -r .sha1)
if [ -z "$api_token" ] || [ "$api_token" = "null" ]; then
  log "ERROR: failed to mint token: $token_response"
  exit 1
fi
log "minted token"

# Create organization. 422 = already exists; treat as success.
log "creating organization $FORGEJO_ORG"
http_code=$(
  curl -fsS -o /dev/null -w '%{http_code}' \
    -H "Authorization: token $api_token" \
    -H 'Content-Type: application/json' \
    -X POST \
    -d "{\"username\":\"$FORGEJO_ORG\",\"visibility\":\"public\"}" \
    "$FORGEJO_URL/api/v1/orgs" || true
)
if [ "$http_code" != "201" ] && [ "$http_code" != "422" ]; then
  log "ERROR: create org status=$http_code"
  exit 1
fi

# Create repo with auto_init so we have a default branch to push to.
log "creating repo $FORGEJO_ORG/$FORGEJO_REPO"
http_code=$(
  curl -fsS -o /dev/null -w '%{http_code}' \
    -H "Authorization: token $api_token" \
    -H 'Content-Type: application/json' \
    -X POST \
    -d "{\"name\":\"$FORGEJO_REPO\",\"auto_init\":true,\"default_branch\":\"main\"}" \
    "$FORGEJO_URL/api/v1/orgs/$FORGEJO_ORG/repos" || true
)
if [ "$http_code" != "201" ] && [ "$http_code" != "409" ]; then
  log "ERROR: create repo status=$http_code"
  exit 1
fi

# Make sure Actions is enabled (it usually is by default, but be explicit).
log "enabling Actions on $FORGEJO_ORG/$FORGEJO_REPO"
curl -fsS -o /dev/null \
  -H "Authorization: token $api_token" \
  -H 'Content-Type: application/json' \
  -X PATCH \
  -d '{"has_actions":true}' \
  "$FORGEJO_URL/api/v1/repos/$FORGEJO_ORG/$FORGEJO_REPO"

# Commit .forgejo/workflows/echo.yml — the push auto-queues a workflow run.
container_block=""
if [ -n "${FORGEJO_WORKFLOW_CONTAINER_OPTS:-}" ]; then
  # The two-space leading indent keeps the block at job level under `hello:`.
  container_block=$'\n    container:\n      image: alpine:3.19\n      options: '"$FORGEJO_WORKFLOW_CONTAINER_OPTS"
fi
workflow_yaml=$(cat <<EOF
name: echo
on: push
jobs:
  hello:
    runs-on: $FORGEJO_LABEL$container_block
    steps:
      - run: echo "hello from fj-bellows"
      # Regression for #41: docker_host: automount in the runner config makes
      # the runner mount /var/run/docker.sock into every spawned job container,
      # so DinD steps work. If that ever regresses, this step exits 1, the
      # workflow fails, the orchestrator never logs "job complete", and the
      # e2e times out — surfacing the regression.
      - name: docker socket is mounted into job container
        run: test -S /var/run/docker.sock
EOF
)
content_b64=$(printf '%s' "$workflow_yaml" | base64 | tr -d '\n')

log "committing .forgejo/workflows/echo.yml"
http_code=$(
  curl -fsS -o /dev/null -w '%{http_code}' \
    -H "Authorization: token $api_token" \
    -H 'Content-Type: application/json' \
    -X POST \
    -d "{\"branch\":\"main\",\"content\":\"$content_b64\",\"message\":\"add workflow\"}" \
    "$FORGEJO_URL/api/v1/repos/$FORGEJO_ORG/$FORGEJO_REPO/contents/.forgejo/workflows/echo.yml" || true
)
if [ "$http_code" != "201" ] && [ "$http_code" != "422" ]; then
  log "ERROR: commit workflow status=$http_code"
  exit 1
fi

# The job appears in the queue after Forgejo parses the push; poll until it's
# visible (or fail with a clear timeout message).
log "waiting for job to be queued"
queued=0
for i in $(seq 1 30); do
  count=$(
    curl -fsS \
      -H "Authorization: token $api_token" \
      "$FORGEJO_URL/api/v1/repos/$FORGEJO_ORG/$FORGEJO_REPO/actions/runners/jobs?labels=$FORGEJO_LABEL" |
    jq 'if . == null then 0 else length end'
  )
  if [ "$count" -ge 1 ]; then
    log "job queued after ${i}s"
    queued=1
    break
  fi
  sleep 1
done
if [ "$queued" -ne 1 ]; then
  log "ERROR: job was not queued within 30s"
  exit 1
fi

# Print the token on stdout so the caller can capture it.
printf '%s\n' "$api_token"
