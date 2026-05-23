#!/usr/bin/env bash
# Assert end-to-end behavior of fj-bellows against a seeded Forgejo (v15+).
#
# We verify the full happy path:
#   (1) fj-bellows decodes the live /actions/runners/jobs response
#       (no "poll waiting jobs" errors in the orchestrator log).
#   (2) A worker container labelled fj-bellows.tag=<tag> is provisioned.
#   (3) The orchestrator registers an ephemeral runner, dispatches the queued
#       job via `forgejo-runner one-job --handle`, and logs "job complete".
#   (4) The per-second billing idle-timeout tears the worker down.
#
# Required env:
#   FJBELLOWS_LOG    path to fj-bellows stderr capture
#   FJBELLOWS_TAG    instance tag the daemon stamps on workers
# Optional:
#   PROVISION_WAIT   seconds to wait for the worker container to appear (default 60)
#   JOB_WAIT         seconds to wait for "job complete" after provisioning (default 180)
#   TEARDOWN_WAIT    seconds to wait for idle teardown (default 30)

set -euo pipefail

log() { printf '[assert] %s\n' "$*" >&2; }

: "${FJBELLOWS_LOG:?}"
: "${FJBELLOWS_TAG:?}"
: "${PROVISION_WAIT:=60}"
: "${JOB_WAIT:=180}"
: "${TEARDOWN_WAIT:=30}"

failures=0
fail() { log "FAIL: $*"; failures=$((failures + 1)); }
pass() { log "PASS: $*"; }

# (1) Reconcile loop must not log a "poll waiting jobs" error: that would mean
# the live Forgejo response failed to decode against our types.
if grep -E 'poll waiting jobs' "$FJBELLOWS_LOG" >/dev/null 2>&1; then
  fail "fj-bellows logged 'poll waiting jobs' errors (live decode failed); see $FJBELLOWS_LOG"
else
  pass "fj-bellows decoded the live /actions/runners/jobs response without error"
fi

# (2) A worker container must have been created with the expected tag label.
log "waiting up to ${PROVISION_WAIT}s for a worker container labelled fj-bellows.tag=$FJBELLOWS_TAG"
seen=0
for i in $(seq 1 "$PROVISION_WAIT"); do
  count=$(docker ps -a --filter "label=fj-bellows.tag=$FJBELLOWS_TAG" --format '{{.ID}}' | wc -l | tr -d ' ')
  if [ "$count" -ge 1 ]; then
    log "worker container observed after ${i}s (count=$count)"
    seen=1
    break
  fi
  sleep 1
done
if [ "$seen" -ne 1 ]; then
  fail "no worker container with label fj-bellows.tag=$FJBELLOWS_TAG appeared within ${PROVISION_WAIT}s"
  log "$failures assertion(s) failed; later assertions skipped because they depend on this"
  exit 1
else
  pass "worker container created with label fj-bellows.tag=$FJBELLOWS_TAG"
fi

# (3) The orchestrator must dispatch the job via one-job --handle and complete
# it. The "job complete" Info line is emitted only after RunJob returns success
# (token written, ephemeral runner registered, one-job --wait exited 0).
log "waiting up to ${JOB_WAIT}s for 'job complete' in orchestrator log"
done=0
for i in $(seq 1 "$JOB_WAIT"); do
  if grep -E 'job complete' "$FJBELLOWS_LOG" >/dev/null 2>&1; then
    log "'job complete' observed after ${i}s"
    done=1
    break
  fi
  sleep 1
done
if [ "$done" -ne 1 ]; then
  log "no 'job complete' line; recent orchestrator log tail:"
  tail -n 30 "$FJBELLOWS_LOG" >&2 || true
  fail "ephemeral one-job dispatch did not report completion within ${JOB_WAIT}s"
else
  pass "ephemeral one-job dispatch completed (job complete logged)"
fi

# (4) The per-second billing idle timeout must reclaim the worker. The pool's
# idle_timeout in the e2e-docker config is short (~5s); destroy should happen
# well within the wait.
log "waiting up to ${TEARDOWN_WAIT}s for the idle worker to be torn down"
torn=0
for i in $(seq 1 "$TEARDOWN_WAIT"); do
  running=$(docker ps --filter "label=fj-bellows.tag=$FJBELLOWS_TAG" --format '{{.ID}}' | wc -l | tr -d ' ')
  if [ "$running" -eq 0 ]; then
    log "no running worker after ${i}s (count=0)"
    torn=1
    break
  fi
  sleep 1
done
if [ "$torn" -ne 1 ]; then
  fail "idle worker was not torn down within ${TEARDOWN_WAIT}s"
else
  pass "idle worker reclaimed by per-second teardown policy"
fi

if [ "$failures" -gt 0 ]; then
  log "$failures assertion(s) failed"
  exit 1
fi
log "all assertions passed"
