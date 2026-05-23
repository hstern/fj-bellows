# test/e2e-linode

End-to-end test for the **Linode provider** — provisions a real Linode VM via
fj-bellows, runs an ephemeral `one-job` against a local Forgejo, then tears the
VM down. Distinct from `test/e2e-docker`, which exercises the **docker**
provider in CI.

## How it works

1. A Forgejo v15 service container runs locally (or in CI), published on
   `127.0.0.1:3000` and seeded by `test/e2e-docker/seed.sh` with an admin,
   token, org, repo, and a workflow whose job runs in a container with
   `--network host` so step-container traffic terminates on the same loopback
   the dispatcher's reverse tunnel forwards to.
2. fj-bellows polls Forgejo, sees the queued job, and provisions a Linode
   nanode in `us-ord` (cloud-init installs Docker + forgejo-runner).
3. The orchestrator waits for SSH on the new VM and runs `forgejo-runner
   one-job` over it. The dispatcher opens a reverse port-forward on the same
   SSH session (`internal/orchestrator/dispatch.go`) so the worker's
   `127.0.0.1:3000` reaches the orchestrator-side Forgejo — no out-of-process
   tunnel needed. The runner's step container (sharing the VM's network
   namespace via `--network host`) reaches Forgejo via the same loopback URL.
4. Job completes (orchestrator logs `job complete`). The E2E config sets
   `poll.billing_hour: 60s, poll.hour_margin: 10s`, so the orchestrator's
   hourly-cycle teardown fires within ~50s of the next cycle boundary and the
   Linode is destroyed.
5. Cleanup destroys any leaked instance carrying the run's tag, stops
   fj-bellows, and removes the Forgejo container — on **every** exit path
   including failure and SIGINT.

## Local: `run-local.sh`

```sh
echo "$YOUR_LINODE_PAT" > ~/.linode.pat   # Linodes: R/W + Firewalls: R/W
chmod 600 ~/.linode.pat
test/e2e-linode/run-local.sh
```

- Cost ceiling: one paid hour on `g6-nanode-1` (~$0.0075).
- A pre-flight cleanup destroys any prior `fj-bellows-e2e-local-*` instances.
- Requires `docker`, `ssh`, `ssh-keygen`, `curl`, `jq`, `go` on PATH.
- The token file path is `~/.linode.pat` by default; override with
  `LINODE_PAT_FILE=/path/to/pat`.

## CI: `e2e-linode` job

The CI version lives in `.github/workflows/ci.yml` as the `e2e-linode` job.

- **Trigger**: push to `main`, tag pushes, and manual `workflow_dispatch`.
  Pull-request events are skipped to avoid spending ~1¢ per PR push.
- **Gate**: the `LINODE_E2E_TOKEN` secret existing. Without it the job skips
  with no spend — so the workflow can be merged before the secret is
  configured.
- **Required check**: added to branch-protection on `main`, so a failing
  `e2e-linode` blocks PR merge alongside `test` and `lint`. (When the secret
  isn't set, the job skips and counts as success for branch protection.)
- **Publish gate**: `publish` will not run if `e2e-linode` failed. A skip is
  fine — publish proceeds when the Linode secret isn't configured.
- **Cost per real run**: ~1¢ (one paid hour on `g6-nanode-1`). Only push to
  `main`, tag pushes, and manual dispatches incur cost; PR pushes are skipped.
- **Cleanup**: an `if: always()` step lists Linodes by the run's tag and
  destroys any survivor, complementing fj-bellows' own `-destroy-on-exit`.

### Idle teardown trade-off

Linode bills whole hours rounded up, so by default the orchestrator keeps a
warm worker until `:55` of the paid hour — maximizing reuse of the already-paid
hour. The E2E job (both local and CI) overrides that by setting a very short
`poll.billing_hour` (60s) so the kill-mark fires inside the test budget. **The
Linode is still billed for one whole hour on Linode's side**; we're choosing
to close earlier (sacrificing the fill-the-paid-hour benefit) so the test can
actually observe end-to-end teardown. After "job complete" the E2E waits up to
~120s for `destroyed idle node` in the log and confirms the Linode is gone
from the Linode API.

This is the only place `billing_hour` is shortened — production deployments
should leave it at the default `1h` to fully use each paid hour. The teardown
policy's pure logic is covered by `internal/orchestrator/teardown_test.go`.
