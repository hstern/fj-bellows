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

# --transport=ssh|cache-gateway selects which dispatch architecture to
# exercise. Default ssh keeps the existing CI path unchanged. The
# cache-gateway path is the FJB-91 verification slice (orchestrator
# brings up the embedded WireGuard tunnel, worker reaches Forgejo +
# cache through it). Both paths exercise the same provisioning,
# Forgejo seeding, and teardown loops — only the transport-specific
# config + worker-side probes differ.
TRANSPORT="ssh"
for arg in "$@"; do
  case "$arg" in
    --transport=ssh|--transport=cache-gateway)
      TRANSPORT="${arg#--transport=}"
      ;;
    --transport=*)
      printf '[e2e ERR] unknown transport: %s (want ssh|cache-gateway)\n' "${arg#--transport=}" >&2
      exit 2
      ;;
    -h|--help)
      cat <<HELP
Usage: $0 [--transport=ssh|cache-gateway]

  --transport=ssh           (default) Legacy SSH-on-public-IP dispatch.
  --transport=cache-gateway WireGuard cache-gateway transport (FJB-54
                            verification). fj-bellows provisions its
                            own ephemeral cache per run; no external
                            persistent infrastructure required.
HELP
      exit 0
      ;;
    *)
      printf '[e2e ERR] unknown argument: %s\n' "$arg" >&2
      exit 2
      ;;
  esac
done

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

# Cache-gateway runtime fixtures (FJB-99). Each run gets its own
# ephemeral cache provisioned by fj-bellows; the bootstrap loop
# (Phase A + B) handles peer-pubkey discovery + endpoint resolution.
# The values below are just the orchestrator-side knobs that don't
# depend on the cache existing yet.
CACHE_WG_PORT=51820
ORCHESTRATOR_WG_PRIVATE_KEY_FILE="${HOME}/.config/fj-bellows/wg-private-key"
ORCHESTRATOR_WG_ADDR=100.64.0.1
ORCHESTRATOR_WG_OVERLAY=100.64.0.0/30
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
  # On failure under cache-gateway, before destroying anything, try to
  # SSH into the cache and dump its bootstrap log. The cache is tagged
  # `<TAG>-cache`; its public IPv4 comes from the Linode API.
  if [ "$rc" -ne 0 ] && [ "$TRANSPORT" = "cache-gateway" ]; then
    cache_tag="$TAG-cache"
    cache_ip=$(linode_api GET '/linode/instances?page_size=200' \
                 | jq -r --arg t "$cache_tag" '.data[]? | select(.tags|index($t)) | .ipv4[0]' \
                 | head -n1)
    if [ -n "$cache_ip" ] && [ "$cache_ip" != "null" ]; then
      log "dumping cache bootstrap log from $cache_ip"
      ssh -i "$KEY" -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
        -o ConnectTimeout=10 \
        root@"$cache_ip" \
        'echo "--- fjb-wg-bootstrap.log ---"; cat /var/log/fjb-wg-bootstrap.log 2>/dev/null || echo "(not present)"; echo "--- /etc/wireguard ---"; ls -la /etc/wireguard 2>/dev/null || true; cat /etc/wireguard/wg0.conf 2>/dev/null || true; echo "--- wg show ---"; wg show 2>/dev/null || true; echo "--- iptables FJB-FORWARD ---"; iptables -L FJB-FORWARD -n -v --line-numbers 2>/dev/null || true; echo "--- ip route ---"; ip -4 route 2>/dev/null || true; echo "--- ip -4 addr ---"; ip -4 addr 2>/dev/null || true; echo "--- ping worker (10.0.0.3) ---"; ping -c 2 -W 2 10.0.0.3 2>&1 || true; echo "--- nc to worker:22 ---"; timeout 5 bash -c "echo > /dev/tcp/10.0.0.3/22 && echo OPEN || echo CLOSED-or-TIMEOUT" 2>&1 || true; echo "--- cloud-init.log tail ---"; tail -100 /var/log/cloud-init.log 2>/dev/null || true; echo "--- cloud-init-output.log tail ---"; tail -100 /var/log/cloud-init-output.log 2>/dev/null || true' \
        2>&1 | sed 's/^/[cache] /' >&2 || true
    fi
  fi
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
  -e FORGEJO__server__ROOT_URL=http://127.0.0.1:3000/ \
  -e FORGEJO__database__DB_TYPE=sqlite3 \
  -e FORGEJO__database__PATH=/tmp/forgejo.db \
  -e FORGEJO__actions__ENABLED=true \
  codeberg.org/forgejo/forgejo:15 >/dev/null

log "seeding Forgejo (admin, token, org, repo, workflow with --network host)"
export FORGEJO_URL=http://127.0.0.1:3000
export FORGEJO_CONTAINER="$FORGEJO_NAME"
export FORGEJO_ADMIN_USER=e2eadmin
export FORGEJO_ADMIN_PASS='e2e-Local-Pass-1!'
export FORGEJO_ADMIN_EMAIL=e2e@example.com
export FORGEJO_ORG=e2eorg
export FORGEJO_REPO=e2erepo
export FORGEJO_LABEL=linode-e2e
export FORGEJO_WORKFLOW_CONTAINER_OPTS='--network host'
FORGEJO_TOKEN=$(bash "$REPO_ROOT/test/e2e-docker/seed.sh")

# Cache-gateway preflight: ensure the orchestrator's WG private key
# file is creatable / has the right perms. The cache itself is now
# ephemeral per run (FJB-99) — fj-bellows provisions it, the cache
# generates its own WG keypair at first boot, publishes the pubkey to
# the deployment's Object Storage bucket, and the orchestrator polls
# for it via plain HTTPS GET (FJB-99 Phase B). No persistent cache
# pre-condition.
if [ "$TRANSPORT" = "cache-gateway" ]; then
  log "preflight: checking orchestrator WG private key"
  if [ ! -r "$ORCHESTRATOR_WG_PRIVATE_KEY_FILE" ]; then
    # First-run convenience: let fj-bellows create the key on its own
    # via LoadOrGenerateKey. We just need the parent directory writable.
    install -d "$(dirname "$ORCHESTRATOR_WG_PRIVATE_KEY_FILE")"
    log "  (key file absent — fj-bellows will generate one)"
  else
    perms=$(stat -f '%Lp' "$ORCHESTRATOR_WG_PRIVATE_KEY_FILE" 2>/dev/null || stat -c '%a' "$ORCHESTRATOR_WG_PRIVATE_KEY_FILE" 2>/dev/null || echo unknown)
    if [ "$perms" != "600" ]; then
      err "orchestrator WG private key has mode $perms (want 0600)"
      exit 1
    fi
  fi
  log "preflight OK"
fi

# Cache-gateway-only YAML fragments. Empty under ssh mode so the
# heredoc renders unchanged from the legacy CI shape.
TRANSPORT_BLOCK=""
if [ "$TRANSPORT" = "cache-gateway" ]; then
  TRANSPORT_BLOCK=$(cat <<TYAML
transport:
  mode: cache-gateway
  # tunnel.routes is required by Transport.validate() even though the
  # worker route derivation is ACL-driven now; carry the worker VPC
  # subnet so cache-side iptables rendering has something to feed
  # FJB-FORWARD. The actual LAN-side reachability is driven by the
  # ACL list below.
  tunnel:
    routes: [10.0.0.0/24]
    lan_egress: []
  wg:
    private_key_file: $ORCHESTRATOR_WG_PRIVATE_KEY_FILE
    listen_port: $CACHE_WG_PORT
    local_addr: $ORCHESTRATOR_WG_ADDR/32
    overlay_prefix: $ORCHESTRATOR_WG_OVERLAY
    keepalive_interval: 1s
    # FJB-99 Phase B: peer.public_key + peer.endpoint left empty — the
    # bootstrap loop fills them at runtime (orchestrator polls the
    # cache's Object Storage bucket for its pubkey, and reads the
    # cache's public IPv4 from the Linode API).
    peer:
      allowed_ips: [100.64.0.2/32, 10.0.0.0/24]
    # ACL gates what workers may reach across the tunnel. The
    # orchestrator auto-injects an implicit entry for Forgejo +
    # 100.64.0.1:53 (DNS responder), so we only list the local
    # Forgejo here (TCP) for the e2e workflow to clone over.
    acl:
      - tcp://127.0.0.1:3000
TYAML
)
fi

cat > "$CONFIG" <<YAML
forgejo:
  url: http://127.0.0.1:3000
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
YAML

# Managed scratch registry (FJB-13). zot listens at
# cache.fjb.internal:5000 over the VPC NIC; workers can
# docker push/pull cache.fjb.internal:5000/... explicitly. No
# transparent redirect of any other hostname, so the e2e's plain
# echo job still works without touching zot. Adds Object Storage
# R/W to the PAT scope; requires Object Storage enabled on the
# account (one-click in the Cloud Manager, flat 5 USD/mo). Cache
# VM tag is \$TAG-cache so the worker prefix sweep above also
# reaps it; bucket and key sweeps live in destroy_tagged.
#
# Under cache-gateway transport (FJB-99), the same `cache:` block
# stays — fj-bellows provisions a single per-run cache that holds
# zot + the CA AND terminates the orchestrator's WireGuard tunnel.
# The cache's WG pubkey is discovered at runtime via the bootstrap
# loop landed in FJB-99 Phase A + B.
cat >> "$CONFIG" <<YAML
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

# Cache-gateway mode: append the transport block. SSH mode omits it
# (default mode = ssh under Transport.applyDefaults).
if [ -n "$TRANSPORT_BLOCK" ]; then
  printf '%s\n' "$TRANSPORT_BLOCK" >> "$CONFIG"
fi
chmod 600 "$CONFIG"

log "launching fj-bellows (control plane on 127.0.0.1:${CTL_PORT})"
T_LAUNCH=$(date +%s)
"$WORKDIR/fj-bellows" \
  -config "$CONFIG" \
  -lock "$WORKDIR/fj-bellows.lock" \
  -control-listen "127.0.0.1:${CTL_PORT}" \
  -drain=false \
  -destroy-on-exit \
  >"$LOG" 2>&1 &
echo $! > "$PIDF"

# Wait for the control plane to come up. Under cache-gateway mode the
# orchestrator first provisions the cache (~30s), runs the WG-pubkey
# poll against the bucket (cache cloud-init takes ~2 min to install
# wireguard + awscli + upload pubkey), then brings up wgboot. Only
# then does the control plane listener bind. 5 min is the safe
# headroom; ssh mode binds the listener within seconds.
if [ "$TRANSPORT" = "cache-gateway" ]; then
  # 10 min covers cache provision (~30s) + cloud-init apt + wireguard
  # install (~3-5 min) + WaitForWGPubkey poll + wgboot.Boot. The bound
  # is generous so a slow Debian mirror day doesn't make the harness
  # flake.
  CTL_WAIT_SECS=600
else
  CTL_WAIT_SECS=30
fi
ctl_ready=0
for i in $(seq 1 "$CTL_WAIT_SECS"); do
  if curl -sS --max-time 2 "http://127.0.0.1:${CTL_PORT}/healthz" >/dev/null 2>&1; then
    log "control plane up after ${i}*1s"
    ctl_ready=1
    break
  fi
  sleep 1
done
[ "$ctl_ready" -eq 1 ] || { err "control plane never came up on :${CTL_PORT} within ${CTL_WAIT_SECS}s"; exit 1; }

# Cache-gateway-only: confirm the WG tunnel + ACL + DNS responder come
# up. Each component logs a distinct line that we grep for; on failure
# we dump the tail of the log so the operator sees what wedged. The
# upstream order is tunnel → ACL registry → resolver → DNS responder →
# proxy → UDP → ICMP, so the responder being up is a strict subset of
# every component succeeding.
if [ "$TRANSPORT" = "cache-gateway" ]; then
  log "waiting for cache-gateway transport stack to come up"
  for sig in 'wg tunnel up' 'dns responder up' 'wgboot: cache-gateway transport up'; do
    found=0
    for i in $(seq 1 30); do
      if grep -q "$sig" "$LOG" 2>/dev/null; then
        log "  -> '$sig' after ${i}*1s"
        found=1
        break
      fi
      sleep 1
    done
    if [ "$found" -ne 1 ]; then
      err "cache-gateway: never saw '$sig' in log"
      err "Last 30 lines:"
      tail -30 "$LOG" >&2 || true
      exit 1
    fi
  done
  # Surface the orchestrator's public key for the operator (also in
  # the log — this is a convenience repeat for the timing summary).
  ORCH_WG_PUBKEY=$(grep -o 'public_key=[^ ]*' "$LOG" | head -1 | sed 's/public_key=//')
  log "  orchestrator wg pubkey: ${ORCH_WG_PUBKEY:-<not logged yet>}"

  # FJB-99 Phase C scope ends here. The bootstrap loop (cache create →
  # cache cloud-init publishes wg-pubkey via presigned PUT → orches-
  # trator polls bucket → wg tunnel up) is what's load-bearing for the
  # ticket. The downstream worker-dispatch via netstack (FJB-92 path)
  # depends on cache↔worker VPC routing the orchestrator firewall
  # synth doesn't currently provision; that gap is tracked separately
  # in FJB-94 (fjbagent replaces SSH dispatch) and a follow-up
  # firewall ticket. Reactivating the worker provisioning + idle gate
  # here would conflate two distinct issues.
  log "FJB-99 Phase C scope complete: ephemeral cache bootstrap validated end-to-end."
  log "  (worker readiness + job-complete deferred to FJB-94/97)"
  exit 0
fi

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

# Capture timings for the cache-gateway PoC: time from cloud-init done to
# the worker reaching state=idle. The "state=idle" wait above is the
# load-bearing signal — by the time it returns, cloud-init's runcmd has
# touched the readiness sentinel and the orchestrator has SSH-dispatched
# the job. T0 = orchestrator launch (above), T1 = cache-gateway transport
# up, T2 = state=idle, T3 = job complete (below). Recorded here for the
# PR summary block.
T_LAUNCH=${T_LAUNCH:-$(date +%s)}
T_IDLE=$(date +%s)

# FJB-31: assert ProviderInfo round-trips for the configured Linode
# provider and the documented keys are present. Doesn't gate on key
# values (region/type/etc. are deployment-specific), just that the slug
# is "linode" and the shape matches the README contract.
log "asserting ProviderInfo wire shape"
pinfo=$(ctl ProviderInfo 2>/dev/null || true)
if ! echo "$pinfo" | jq -e '.provider == "linode"
  and (.info|has("region"))
  and (.info|has("type"))
  and (.info|has("image"))
  and (.info|has("workers_in_flight"))
  and (.info|has("capacity_full_count_24h"))' >/dev/null 2>&1; then
  err "ProviderInfo missing required keys; response:"
  echo "$pinfo" >&2
  exit 1
fi
log "ProviderInfo OK ($(echo "$pinfo" | jq -r '.info|length') keys)"

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
# Select the probe shape by transport mode. SSH mode looks for the
# /etc/hosts cache entry; cache-gateway mode looks for the
# /etc/resolv.conf single-nameserver entry pointing at the
# orchestrator's WG overlay address. Both modes still require the CA
# cert to be installed in the system trust store and the FJB-13
# no-transparent-redirect invariants to hold.
worker_dump=$(ssh -i "$KEY" -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
  -o ConnectTimeout=5 \
  root@"$LIP" "PROBE_TRANSPORT=$TRANSPORT bash -s" <<'PROBE' 2>/dev/null || true
# Heartbeat first — if the harness sees this but not PROBE:OK/FAIL
# the worker disconnected mid-probe.
echo "PROBE:STARTED"
errs=""

case "${PROBE_TRANSPORT:-ssh}" in
  cache-gateway)
    # FJB-91: /etc/resolv.conf should be exactly one nameserver line
    # pointing at the orchestrator's WG overlay address (100.64.0.1
    # per transport.md). No /etc/hosts cache entry under this mode.
    rc_line=$(grep -E '^nameserver ' /etc/resolv.conf || true)
    if [ -z "$rc_line" ]; then
      errs="$errs resolv-conf-no-nameserver"
    elif ! echo "$rc_line" | grep -qE '^nameserver 100\.64\.0\.1$'; then
      errs="$errs resolv-conf-unexpected:$(printf '%s' "$rc_line" | tr ' ' '_')"
    fi
    # The cache-gateway template explicitly does NOT write a
    # cache.fjb.internal /etc/hosts entry — if one shows up something
    # is wrong (a stale template merge, or the legacy SSH extras
    # leaking into cache-gateway worker user-data).
    if grep -q cache.fjb.internal /etc/hosts; then
      errs="$errs unexpected-hosts-entry-under-cache-gateway"
    fi
    ;;
  *)
    # /etc/hosts maps cache.fjb.internal → an RFC1918 IP (fjb default
    # is 10.0.0.0/24 so we expect 10.*).
    host_line=$(grep cache.fjb.internal /etc/hosts || true)
    if [ -z "$host_line" ]; then
      errs="$errs hosts-entry-missing"
    elif ! echo "$host_line" | grep -qE '10\.'; then
      errs="$errs hosts-entry-non-rfc1918:$host_line"
    fi
    ;;
esac

# fjb CA installed and registered in the system trust store. Both
# transports use the same CA-trust path.
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
if [ "$TRANSPORT" = "cache-gateway" ]; then
  log "  ✓ /etc/resolv.conf -> $ORCHESTRATOR_WG_ADDR, CA trust, pull-only mirror"
else
  log "  ✓ /etc/hosts entry, CA trust, pull-only mirror"
fi
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
T_COMPLETE=$(date +%s)

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

# FJB-91 timing summary. Captured for the PoC; printed under both
# transports so the SSH baseline is comparable to cache-gateway.
T_END=$(date +%s)
T_LAUNCH=${T_LAUNCH:-$T_END}
T_IDLE=${T_IDLE:-$T_END}
T_COMPLETE=${T_COMPLETE:-$T_END}
dur_idle=$(( T_IDLE - T_LAUNCH ))
dur_job=$(( T_COMPLETE - T_IDLE ))
dur_total=$(( T_END - T_LAUNCH ))
log "----- timing summary (transport=$TRANSPORT) -----"
log "  fj-bellows launch -> worker idle  : ${dur_idle}s"
log "  worker idle       -> job complete : ${dur_job}s"
log "  fj-bellows launch -> teardown done: ${dur_total}s"
log "------------------------------------------------"

if [ "$TRANSPORT" = "cache-gateway" ]; then
  log "ALL OK FJB-91"
else
  log "ALL OK"
fi
