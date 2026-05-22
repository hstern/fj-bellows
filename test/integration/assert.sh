#!/usr/bin/env bash
# Assert end-to-end behavior of fj-bellows against a seeded Forgejo.
#
# The asserts are scoped to what's verifiable against the currently-released
# Forgejo (<= 12) without the Forgejo 15+ ephemeral runner + --handle support
# that the orchestrator's job-dispatch path depends on. Specifically we verify
# that fj-bellows:
#   (1) decodes the live /actions/runners/jobs response (the orchestrator's
#       reconcile loop logs do not print decode errors), and
#   (2) attempts to provision a worker container (a docker container labelled
#       fj-bellows.tag=<tag> appears).
#
# What we deliberately do NOT assert (would require Forgejo >= 15):
#   - the workflow run completes successfully;
#   - an ephemeral runner registers.
# See test/integration/README.md for the rationale.
#
# Required env:
#   FJBELLOWS_LOG    path to fj-bellows stderr capture
#   FJBELLOWS_TAG    instance tag the daemon stamps on workers
#   PROVISION_WAIT   how long (seconds) to wait for a worker container

set -euo pipefail

log() { printf '[assert] %s\n' "$*" >&2; }

: "${FJBELLOWS_LOG:?}"
: "${FJBELLOWS_TAG:?}"
: "${PROVISION_WAIT:=120}"

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

# (1b) The reconcile loop must observe the queued job at least once. Look for
# a provision attempt OR a register-ephemeral attempt — both indicate that the
# orchestrator saw the job, filtered it as serviceable, and tried to act on it.
if grep -E 'provisioned|register ephemeral runner|provision' "$FJBELLOWS_LOG" >/dev/null 2>&1; then
  pass "fj-bellows acted on the queued job (provisioned / attempted to register)"
else
  fail "fj-bellows did not act on the queued job; expected a 'provision' or 'register ephemeral runner' log line"
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
else
  pass "worker container created with label fj-bellows.tag=$FJBELLOWS_TAG"
fi

if [ "$failures" -gt 0 ]; then
  log "$failures assertion(s) failed"
  exit 1
fi
log "all assertions passed"
