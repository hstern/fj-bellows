#!/usr/bin/env bash
# Local end-to-end driver for the Linode provider.
#
# Provisions a real Linode (~1¢), runs the full ephemeral one-job path against
# a locally-hosted Forgejo, and tears everything down on exit. Use this to
# develop the Linode E2E without going through CI; the CI job (workflow_dispatch
# in .github/workflows/ci.yml) follows the same shape.
#
# Prerequisites:
#   - docker, ssh, ssh-keygen, curl, jq, go on PATH.
#   - A Linode personal access token in ~/.linode.pat (Linodes: Read/Write).
#   - Local TCP port 3000 free (we publish Forgejo on 127.0.0.1:3000).
#
# Cost ceiling: one paid hour on a g6-nanode-1 (~$0.0075). A pre-flight cleanup
# destroys any Linodes left tagged fj-bellows-e2e-local-* from prior runs.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
TOKEN_FILE="${LINODE_PAT_FILE:-$HOME/.linode.pat}"
WORKDIR=$(mktemp -d -t fjb-e2e-XXXXXX)
TAG="fj-bellows-e2e-local-$(date +%s)-$$"
KEY="$WORKDIR/id_ed25519"
KNOWN="$WORKDIR/known_hosts"
CONFIG="$WORKDIR/config.yaml"
LOG="$WORKDIR/fj-bellows.log"
PIDF="$WORKDIR/fj-bellows.pid"
# macOS sun_path is 104 chars. Honor $TMPDIR, but fall back to /tmp if the
# prefix would leave too few characters for the socket name.
_tmp="${TMPDIR:-/tmp}"; _tmp="${_tmp%/}"
[ ${#_tmp} -gt 70 ] && _tmp=/tmp
TUNNEL_CTL="$_tmp/fjb-e2e-ssh-$$.sock"
unset _tmp
FORGEJO_NAME="fjb-e2e-forgejo-$$"

log() { printf '[e2e] %s\n' "$*" >&2; }
err() { printf '[e2e ERR] %s\n' "$*" >&2; }

[ -r "$TOKEN_FILE" ] || { err "missing $TOKEN_FILE (set LINODE_PAT_FILE to override)"; exit 1; }
TOKEN=$(tr -d '[:space:]' < "$TOKEN_FILE")
[ -n "$TOKEN" ] || { err "empty token in $TOKEN_FILE"; exit 1; }

linode_api() {
  local method=$1 path=$2; shift 2
  curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
       -X "$method" "https://api.linode.com/v4${path}" "$@"
}

destroy_tagged() {
  local prefix=$1
  local ids
  ids=$(linode_api GET '/linode/instances?page_size=200' 2>/dev/null \
        | jq -r --arg p "$prefix" '.data[]? | select(.tags|any(startswith($p))) | .id' 2>/dev/null || true)
  for id in $ids; do
    log "destroying Linode $id"
    linode_api DELETE "/linode/instances/$id" >/dev/null 2>&1 || true
  done
}

cleanup() {
  local rc=$?
  log "cleanup (rc=$rc)"
  [ -S "$TUNNEL_CTL" ] && ssh -O exit -o ControlPath="$TUNNEL_CTL" placeholder 2>/dev/null || true
  [ -s "$PIDF" ] && kill "$(cat "$PIDF")" 2>/dev/null || true
  docker rm -f "$FORGEJO_NAME" >/dev/null 2>&1 || true
  destroy_tagged "$TAG"
  if [ "$rc" -ne 0 ]; then
    err "FAILED. Workdir kept: $WORKDIR"
    err "Last 50 lines of orchestrator log:"
    tail -50 "$LOG" 2>/dev/null | sed 's/^/[fjb] /' >&2 || true
  else
    log "OK"
    rm -rf "$WORKDIR"
  fi
}
trap cleanup EXIT INT TERM

# Pre-flight: any leaked instances from earlier runs?
log "pre-flight: destroying any Linodes tagged fj-bellows-e2e-local-*"
destroy_tagged "fj-bellows-e2e-local-"

log "building fj-bellows"
(cd "$REPO_ROOT" && go build -o "$WORKDIR/fj-bellows" ./cmd/fj-bellows)

log "generating ed25519 keypair"
ssh-keygen -t ed25519 -N '' -f "$KEY" -C 'fj-bellows-e2e-local' -q

log "starting Forgejo v15 on 127.0.0.1:3000"
docker run -d --rm --name "$FORGEJO_NAME" \
  -p 127.0.0.1:3000:3000 \
  -e FORGEJO__security__INSTALL_LOCK=true \
  -e FORGEJO__server__ROOT_URL=http://localhost:3000/ \
  -e FORGEJO__database__DB_TYPE=sqlite3 \
  -e FORGEJO__database__PATH=/tmp/forgejo.db \
  -e FORGEJO__actions__ENABLED=true \
  codeberg.org/forgejo/forgejo:15 >/dev/null

log "seeding Forgejo (admin, token, org, repo, workflow with --network host)"
export FORGEJO_URL=http://localhost:3000
export FORGEJO_CONTAINER="$FORGEJO_NAME"
export FORGEJO_ADMIN_USER=e2eadmin
export FORGEJO_ADMIN_PASS='e2e-Local-Pass-1!'
export FORGEJO_ADMIN_EMAIL=e2e@example.com
export FORGEJO_ORG=e2eorg
export FORGEJO_REPO=e2erepo
export FORGEJO_LABEL=linode-e2e
export FORGEJO_WORKFLOW_CONTAINER_OPTS='--network host'
FORGEJO_TOKEN=$(bash "$REPO_ROOT/test/e2e-docker/seed.sh")

cat > "$CONFIG" <<YAML
forgejo:
  url: http://localhost:3000
  token: $FORGEJO_TOKEN
  scope: orgs/$FORGEJO_ORG
  labels: [$FORGEJO_LABEL]
tag: $TAG
scale:
  max: 1
provider: linode
provider_config:
  region: us-ord
  type: g6-nanode-1
  image: linode/debian12
  token: $TOKEN
ssh:
  private_key_file: $KEY
  user: root
  port: 22
poll:
  interval: 5s
  idle_timeout: 30s
  hour_margin: 5m
YAML
chmod 600 "$CONFIG"

log "launching fj-bellows"
"$WORKDIR/fj-bellows" \
  -config "$CONFIG" \
  -lock "$WORKDIR/fj-bellows.lock" \
  -drain=false \
  -destroy-on-exit \
  >"$LOG" 2>&1 &
echo $! > "$PIDF"
sleep 2

log "polling Linode API for tag=$TAG"
LIP=""
for i in $(seq 1 180); do
  body=$(linode_api GET '/linode/instances?page_size=200')
  LIP=$(printf '%s' "$body" | jq -r --arg t "$TAG" '.data[]? | select(.tags|index($t)) | .ipv4[0]' | head -n1)
  if [ -n "$LIP" ] && [ "$LIP" != "null" ]; then
    log "Linode IP $LIP after ${i}*2s"
    break
  fi
  sleep 2
done
[ -n "$LIP" ] && [ "$LIP" != "null" ] || { err "Linode did not appear within ~6 min"; exit 1; }

log "waiting for SSH on $LIP"
ssh_ready=0
for i in $(seq 1 180); do
  if ssh -i "$KEY" -o StrictHostKeyChecking=accept-new \
         -o UserKnownHostsFile="$KNOWN" -o ConnectTimeout=3 \
         root@"$LIP" 'true' 2>/dev/null; then
    log "SSH ready after ${i}*2s"
    ssh_ready=1
    break
  fi
  sleep 2
done
[ "$ssh_ready" -eq 1 ] || { err "SSH never came up on $LIP"; exit 1; }

log "opening SSH reverse tunnel (Linode loopback:3000 -> our Forgejo)"
# cloud-init reloads sshd to pick up the injected host key, so the first SSH
# session can land in a window where sshd is restarting and the next connection
# gets reset. Retry a handful of times.
tunnel_up=0
for attempt in 1 2 3 4 5 6 7 8; do
  if ssh -i "$KEY" \
       -o UserKnownHostsFile="$KNOWN" -o StrictHostKeyChecking=no \
       -o ControlMaster=yes -o ControlPath="$TUNNEL_CTL" \
       -o ExitOnForwardFailure=yes \
       -fN -R 3000:localhost:3000 root@"$LIP" 2>/dev/null; then
    log "tunnel up on attempt $attempt"
    tunnel_up=1
    break
  fi
  log "attempt $attempt failed; retrying in 3s"
  sleep 3
done
[ "$tunnel_up" -eq 1 ] || { err "tunnel failed after 8 attempts"; exit 1; }

log "waiting for 'job complete' in orchestrator log (up to ~6 min)"
complete=0
for i in $(seq 1 180); do
  if grep -q 'job complete' "$LOG" 2>/dev/null; then
    log "'job complete' after ${i}*2s"
    complete=1
    break
  fi
  sleep 2
done
if [ "$complete" -ne 1 ]; then
  err "job did not complete; last 30 lines:"
  tail -30 "$LOG" >&2 || true
  exit 1
fi

# Linode's hourly-rounded billing means the warm-hold deliberately keeps the
# worker until :55 of the paid hour, so we do NOT wait for idle teardown here —
# asserting fast teardown would contradict the design. The trap cleanup below
# (and fj-bellows' `-destroy-on-exit`) reclaims the VM on exit.

log "ALL OK"
