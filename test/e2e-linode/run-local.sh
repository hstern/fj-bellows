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
FORGEJO_NAME="fjb-e2e-forgejo-$$"
# Random high port for the control plane so concurrent runs (or runs that
# race with other local services on the default 9876) don't collide.
CTL_PORT=$((30000 + RANDOM % 30000))
CTL_BASE="http://127.0.0.1:${CTL_PORT}/fjbellows.control.v1.ControlService"

# ctl POSTs an empty JSON request to one of the control plane's RPCs. Stdout
# is the response body (JSON). Use `jq` to extract fields.
ctl() {
  curl -sS --max-time 5 -X POST -H 'content-type: application/json' -d '{}' \
       "${CTL_BASE}/$1"
}

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
  # Same prefix sweep for managed firewalls (#26). Instance deletes above
  # must finish first so the firewall has no devices when we DELETE it.
  local fwids
  fwids=$(linode_api GET '/networking/firewalls?page_size=200' 2>/dev/null \
          | jq -r --arg p "$prefix" '.data[]? | select(.tags|any(startswith($p))) | .id' 2>/dev/null || true)
  for id in $fwids; do
    log "destroying managed firewall $id"
    linode_api DELETE "/networking/firewalls/$id" >/dev/null 2>&1 || true
  done
  # Managed VPCs (FJB-6). VPCs have no .tags field; ownership is by label
  # prefix `fj-bellows-<tag>`. Subnets are inline under each VPC and must
  # be deleted before the VPC. Linode auto-detaches Linode interfaces when
  # the underlying instance is deleted, but the subnet DELETE still needs
  # the subnet to have no live interfaces — instance deletes above handle
  # that.
  local vpcids
  vpcids=$(linode_api GET '/vpcs?page_size=200' 2>/dev/null \
           | jq -r --arg p "fj-bellows-$prefix" '.data[]? | select(.label|startswith($p)) | .id' 2>/dev/null || true)
  for vid in $vpcids; do
    local subids
    subids=$(linode_api GET "/vpcs/$vid/subnets?page_size=200" 2>/dev/null \
             | jq -r '.data[]?.id' 2>/dev/null || true)
    for sid in $subids; do
      log "destroying VPC subnet $vid/$sid"
      linode_api DELETE "/vpcs/$vid/subnets/$sid" >/dev/null 2>&1 || true
    done
    log "destroying managed VPC $vid"
    linode_api DELETE "/vpcs/$vid" >/dev/null 2>&1 || true
  done
  # Object Storage scoped access keys (FJB-6 PR 2a). Label is
  # `fj-bellows-cache-<tag>...`; reap any key whose label contains the
  # run's tag prefix so failed runs don't leak keys to the operator's
  # account. Order doesn't matter relative to buckets — keys can be
  # deleted while the bucket is still present.
  local keyids
  keyids=$(linode_api GET '/object-storage/keys?page_size=200' 2>/dev/null \
           | jq -r --arg p "$prefix" '.data[]? | select(.label|contains($p)) | .id' 2>/dev/null || true)
  for kid in $keyids; do
    log "destroying object storage key $kid"
    linode_api DELETE "/object-storage/keys/$kid" >/dev/null 2>&1 || true
  done
  # Object Storage buckets (FJB-6 PR 2a). Label is `fjb-cache-<tag>`.
  # DELETE on a non-empty bucket returns 400; this sweep accepts that
  # (e.g. zot pulled an image during the run and the bucket has data).
  # The bucket then survives until the operator manually empties it —
  # acceptable for tests but flag it so the test author can hand-clean.
  local bktrows
  bktrows=$(linode_api GET '/object-storage/buckets?page_size=200' 2>/dev/null \
            | jq -r --arg p "$prefix" '.data[]? | select(.label|contains($p)) | "\(.region)\t\(.label)"' 2>/dev/null || true)
  while IFS=$'\t' read -r region label; do
    [ -z "$region" ] && continue
    log "destroying object storage bucket $region/$label"
    if ! linode_api DELETE "/object-storage/buckets/$region/$label" >/dev/null 2>&1; then
      log "  (bucket non-empty; manual cleanup may be needed)"
    fi
  done <<< "$bktrows"
}

cleanup() {
  local rc=$?
  log "cleanup (rc=$rc)"
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
  image: linode/debian13
  token: $TOKEN
  # Managed firewall: SSH only from this host's external IP. github-actions
  # is intentionally NOT exercised here — its CIDR list (5000+ v4 today)
  # exceeds Linode's 25-rule-per-firewall cap; tracking that limitation in
  # the followup issue. The PAT in ~/.linode.pat needs Firewalls: R/W in
  # addition to Linodes: R/W.
  firewall:
    allow_inbound:
      - auto
  # Managed VPC (FJB-6). Workers attach to the cache subnet NIC in
  # addition to their public one. The label-prefix sweep in
  # destroy_tagged reclaims the VPC on cleanup so failures do not leak.
  # The PAT in ~/.linode.pat needs VPCs R/W on top of Linodes R/W and
  # Firewalls R/W. (No backticks or colons in heredoc comments: this is
  # an unquoted heredoc so backticks would trigger command substitution
  # and a YAML colon outside a key+value would confuse the parser.)
  vpc:
    subnets:
      cache:
        ipv4: 10.0.0.0/24
  # Managed scratch registry (FJB-13). zot listens at
  # cache.fjb.internal:5000 over the VPC NIC; workers can
  # docker push/pull cache.fjb.internal:5000/... explicitly. No
  # transparent redirect of any other hostname, so the e2e's plain
  # echo job still works without touching zot. Adds Object Storage
  # R/W to the PAT scope; requires Object Storage enabled on the
  # account (one-click in the Cloud Manager, flat 5 USD/mo). Cache
  # VM tag is \$TAG-cache so the worker prefix sweep above also
  # reaps it; bucket and key sweeps live in destroy_tagged.
  cache:
    tls:
      ca_dir: $WORKDIR/cache-ca
ssh:
  private_key_file: $KEY
  user: root
  port: 22
poll:
  interval: 5s
  idle_timeout: 30s
  # Force a short billing cycle so idle teardown fires inside the local-run
  # budget. Linode still bills a whole hour on its side; we're just choosing
  # to close earlier (sacrificing the fill-the-paid-hour benefit) so this
  # driver can actually observe a teardown. With billing_hour=60s and
  # hour_margin=10s the kill mark is created+50s, then +1m50s, etc.
  billing_hour: 60s
  hour_margin: 10s
YAML
chmod 600 "$CONFIG"

log "launching fj-bellows (control plane on 127.0.0.1:${CTL_PORT})"
"$WORKDIR/fj-bellows" \
  -config "$CONFIG" \
  -lock "$WORKDIR/fj-bellows.lock" \
  -control-listen "127.0.0.1:${CTL_PORT}" \
  -drain=false \
  -destroy-on-exit \
  >"$LOG" 2>&1 &
echo $! > "$PIDF"

# Wait for the control plane to come up before we depend on it for state.
ctl_ready=0
for i in $(seq 1 30); do
  if curl -sS --max-time 2 "http://127.0.0.1:${CTL_PORT}/healthz" >/dev/null 2>&1; then
    log "control plane up after ${i}*1s"
    ctl_ready=1
    break
  fi
  sleep 1
done
[ "$ctl_ready" -eq 1 ] || { err "control plane never came up on :${CTL_PORT}"; exit 1; }

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

# The dispatcher opens a reverse port-forward over the dispatch SSH session
# (internal/orchestrator/dispatch.go), so workers reach Forgejo via the
# orchestrator's view of it. No side-car tunnel needed; see #33.

# Wait for a worker to reach state=idle via the control plane's ListWorkers
# RPC (FJB-14 PR2). Replaces the prior `grep -q 'worker ready' $LOG` — the
# pool's state transition is the load-bearing signal, not the log text.
log "waiting for a worker to reach state=idle (up to ~6 min)"
ready=0
for i in $(seq 1 180); do
  if ctl ListWorkers 2>/dev/null | jq -e '.workers[]? | select(.state == "idle")' >/dev/null 2>&1; then
    log "worker state=idle after ${i}*2s"
    ready=1
    break
  fi
  sleep 2
done
if [ "$ready" -ne 1 ]; then
  err "worker did not become idle; last 30 lines of orchestrator log:"
  tail -30 "$LOG" >&2 || true
  err "ListWorkers snapshot:"
  ctl ListWorkers >&2 || true
  exit 1
fi

# Assert via GetCache that the managed cache VM is present and the Linode
# API reports it running. Turns the prior soft-only /v2/ check (which
# still runs as a worker-side probe below) into a fatal gate on the cache
# stack — FJB-15 / FJB-17 expect this.
log "asserting cache VM present + running via GetCache"
cache_ok=0
for i in $(seq 1 60); do
  if ctl GetCache 2>/dev/null | jq -e '.present == true and .vmState == "running"' >/dev/null 2>&1; then
    log "cache present + vm_state=running after ${i}*2s"
    cache_ok=1
    break
  fi
  sleep 2
done
if [ "$cache_ok" -ne 1 ]; then
  err "cache VM not present/running; last GetCache response:"
  ctl GetCache >&2 || true
  exit 1
fi

# FJB-6 PR 3: worker-side cache assertions. Read-only — verifies that
# the PR 2b multipart wrap actually landed the cache plumbing on the
# worker. Runs BEFORE we wait for job-complete so the worker can't be
# idle-reaped mid-probe (the e2e uses billing_hour=60s for fast
# teardown observation, which leaves a tight window post-job-complete).
# One SSH invocation does both the probe and the CA byte-dump (cloud-
# init can still be reconfiguring sshd in the background — a second
# SSH call mid-flight sometimes hits a transient host-key change as
# another sshd reload fires, so we keep this as one round-trip).
# Host-key verification is disabled here (UserKnownHostsFile=/dev/null
# + StrictHostKeyChecking=no) — the orchestrator's real dispatcher
# pins via cloud-init and THAT's the actual security boundary; this
# is just a read probe.
log "worker-side cache assertions"
worker_dump=$(ssh -i "$KEY" -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
  -o ConnectTimeout=5 \
  root@"$LIP" 'bash -s' <<'PROBE' 2>/dev/null || true
# Heartbeat first — if the harness sees this but not PROBE:OK/FAIL
# the worker disconnected mid-probe.
echo "PROBE:STARTED"
errs=""

# /etc/hosts maps cache.fjb.internal → an RFC1918 IP (fjb default is
# 10.0.0.0/24 so we expect 10.*).
host_line=$(grep cache.fjb.internal /etc/hosts || true)
if [ -z "$host_line" ]; then
  errs="$errs hosts-entry-missing"
elif ! echo "$host_line" | grep -qE '10\.'; then
  errs="$errs hosts-entry-non-rfc1918:$host_line"
fi

# fjb CA installed and registered in the system trust store.
if [ ! -s /usr/local/share/ca-certificates/fjb-cache.crt ]; then
  errs="$errs ca-cert-missing"
fi
if ! ls /etc/ssl/certs/ 2>/dev/null | grep -q fjb-cache; then
  errs="$errs ca-cert-not-in-trust-store"
fi

# FJB-13: the worker fragment must NOT ship a containerd hosts.toml
# (the transparent-redirect mechanism was retired) or a daemon.json
# (containerd-snapshotter broke docker push to Forgejo). Assert
# explicitly so a refactor reintroducing either gets caught.
if find /etc/containerd/certs.d -name hosts.toml 2>/dev/null | grep -q .; then
  errs="$errs hosts-toml-present-FJB-13-REGRESSION"
fi
if [ -e /etc/docker/daemon.json ]; then
  errs="$errs daemon-json-present-FJB-13-REGRESSION"
fi

# PROBE:OK / PROBE:FAIL covers ONLY the worker-side plumbing
# assertions above (hosts, CA, no-transparent-redirect). These are
# the load-bearing pieces — they prove the multipart wrap landed
# and the FJB-13 cleanups stuck.
if [ -n "$errs" ]; then
  echo "PROBE:FAIL:$errs"
else
  echo "PROBE:OK"
fi

# Soft check: cache /v2/ reachable from the worker over the VPC NIC,
# with the cert verifying against the installed CA. Logged as
# PROBE:V2: but NOT folded into the fatal PROBE:OK/FAIL — the cache
# cloud-init (apt + download zot + start) can take 3-5 min and the
# e2e's short billing cycle reaps the worker ~30s after job-complete,
# so a slow-cache run would flake here even though the worker
# plumbing is correct. A future PR can add a fjb-side "wait for
# cache ready" signal and turn this back into a fatal check.
v2_ok=0
for i in 1 2 3 4 5; do
  if curl -fsS --max-time 3 https://cache.fjb.internal:5000/v2/ >/dev/null 2>&1; then
    v2_ok=1
    break
  fi
  sleep 2
done
if [ "$v2_ok" -eq 1 ]; then
  echo "PROBE:V2:OK"
else
  echo "PROBE:V2:WARN cache /v2/ not yet reachable (cache cloud-init likely still finishing)"
fi
# Emit the CA PEM with a sentinel so the harness can extract it.
echo "---FJB-CA-BEGIN---"
cat /usr/local/share/ca-certificates/fjb-cache.crt 2>/dev/null || true
echo "---FJB-CA-END---"
PROBE
)
# Filter for the result line — STARTED appears immediately, OK/FAIL at
# the end. `|| true` so a missing match (e.g. SSH died early)
# doesn't blow up under `set -o pipefail`.
probe_line=$(printf '%s\n' "$worker_dump" | grep -E '^PROBE:(OK|FAIL)' | head -1 || true)
if [ "$probe_line" != "PROBE:OK" ]; then
  err "worker-side cache assertions failed: ${probe_line:-<no PROBE result; full dump below>}"
  if printf '%s\n' "$worker_dump" | grep -q '^PROBE:STARTED'; then
    err "(probe started but did not complete — likely SSH dropped mid-run)"
  fi
  err "SSH dump (first 40 lines):"
  printf '%s\n' "$worker_dump" | head -40 >&2
  exit 1
fi
log "  ✓ /etc/hosts entry, CA trust, pull-only mirror"
v2_line=$(printf '%s\n' "$worker_dump" | grep '^PROBE:V2:' | head -1 || true)
case "$v2_line" in
  PROBE:V2:OK)
    log "  ✓ cache /v2/ reachable from worker (TLS verified)"
    ;;
  PROBE:V2:WARN*)
    log "  ⚠ ${v2_line#PROBE:V2:WARN }"
    log "    (non-fatal — cache reachability is a soft check until fjb signals cache-ready)"
    ;;
  *)
    log "  ⚠ no PROBE:V2: line in dump (probe may have been truncated)"
    ;;
esac

# CA byte-equality: extract the PEM between the sentinels and compare
# to the orchestrator's persisted CA.
worker_ca=$(printf '%s\n' "$worker_dump" \
  | awk '/^---FJB-CA-BEGIN---$/{f=1;next} /^---FJB-CA-END---$/{f=0} f')
orch_ca=$(cat "$WORKDIR/cache-ca/ca-cert.pem" 2>/dev/null || true)
if [ -z "$worker_ca" ] || [ -z "$orch_ca" ]; then
  err "CA byte-equality check skipped: worker_ca=${#worker_ca}b orch_ca=${#orch_ca}b"
  exit 1
fi
if [ "$(printf '%s' "$worker_ca")" != "$(printf '%s' "$orch_ca")" ]; then
  err "CA byte mismatch: worker's installed CA differs from orchestrator's persisted CA"
  exit 1
fi
log "  ✓ worker CA byte-identical to orchestrator's persisted CA"

# With assertions green, wait for the runner to finish its job. Detected
# via the control plane: a worker was busy serving the job; either it has
# returned to state=idle with an empty current_job, OR the pool is empty
# (the orchestrator has already reaped the now-idle worker — only possible
# AFTER the job completed, since the reaper never destroys a busy node).
# We've already confirmed at least one worker provisioned successfully via
# the earlier state=idle assertion, so pool-empty here implies job-then-reap,
# not no-worker-ever.
log "waiting for job completion via ListWorkers (up to ~6 min)"
complete=0
for i in $(seq 1 180); do
  resp=$(ctl ListWorkers 2>/dev/null || true)
  if [ -n "$resp" ] && echo "$resp" | jq -e '
        ((.workers // []) | length) == 0
        or (
          all(.workers[]; .state == "idle")
          and all(.workers[]; (.currentJob // "") == "")
        )
      ' >/dev/null 2>&1; then
    log "job complete (all workers idle with no current_job, or pool reaped) after ${i}*2s"
    complete=1
    break
  fi
  sleep 2
done
if [ "$complete" -ne 1 ]; then
  err "job did not complete; last 30 lines of orchestrator log:"
  tail -30 "$LOG" >&2 || true
  err "ListWorkers snapshot:"
  ctl ListWorkers >&2 || true
  exit 1
fi

# Now that billing_hour is configurable, the config above uses a 60s cycle with
# a 10s margin, so the orchestrator destroys an idle worker within ~50s of every
# cycle boundary. Give it ~120s after "job complete" to fire — comfortably above
# one cycle. Linode still bills the whole hour on its side; the trap cleanup and
# `-destroy-on-exit` reclaim the VM regardless.
log "waiting for idle teardown via ListWorkers (up to ~120s)"
teardown=0
for i in $(seq 1 60); do
  # ListWorkers reports an empty pool once the dispatch+teardown goroutine
  # has destroyed the last idle worker. Replaces the prior
  # `grep -q 'destroyed idle node' $LOG`.
  resp=$(ctl ListWorkers 2>/dev/null || true)
  if [ -n "$resp" ] && echo "$resp" | jq -e '(.workers // []) | length == 0' >/dev/null 2>&1; then
    log "ListWorkers empty after ${i}*2s"
    # Confirm the Linode is actually gone from the provider's view.
    body=$(linode_api GET '/linode/instances?page_size=200')
    still=$(printf '%s' "$body" | jq -r --arg t "$TAG" \
            '[.data[]? | select(.tags|index($t))] | length')
    if [ "$still" = "0" ]; then
      log "Linode with tag $TAG is gone from the API"
      teardown=1
      break
    fi
    log "pool reports empty but $still Linode(s) still listed; retrying"
  fi
  sleep 2
done
if [ "$teardown" -ne 1 ]; then
  err "idle teardown did not fire within ~120s; last 30 lines:"
  tail -30 "$LOG" >&2 || true
  exit 1
fi

log "ALL OK"
