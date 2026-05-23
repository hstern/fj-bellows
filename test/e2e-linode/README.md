# test/e2e-linode

End-to-end test for the **Linode provider** — provisions a real Linode VM via
fj-bellows, runs an ephemeral `one-job` against a local Forgejo over an SSH
reverse tunnel, then tears the VM down. Distinct from `test/e2e-docker`, which
exercises the **docker** provider in CI.

## How it works

1. A Forgejo v15 service container runs locally (or in CI), published on
   `127.0.0.1:3000` and seeded by `test/e2e-docker/seed.sh` with an admin,
   token, org, repo, and a workflow whose job runs in a container with
   `--network host` so step-container traffic terminates on the same loopback
   the SSH tunnel will reverse-forward to.
2. fj-bellows polls Forgejo, sees the queued job, and provisions a Linode
   nanode in `us-ord` (cloud-init installs Docker + forgejo-runner).
3. The driver polls the Linode API for the new VM, waits for SSH, and opens
   `ssh -fN -R 3000:localhost:3000 root@<ip>` so the Linode's loopback:3000 is
   the runner's Forgejo.
4. fj-bellows registers an ephemeral runner and runs `forgejo-runner one-job
   --handle` over `docker exec`-equivalent SSH. The runner's step container
   (sharing the Linode's network namespace via `--network host`) reaches
   Forgejo via the same loopback URL.
5. Job completes (orchestrator logs `job complete`). Per-second idle teardown
   reclaims the Linode.
6. Cleanup destroys any leaked instance carrying the run's tag, kills the
   tunnel and fj-bellows, and removes the Forgejo container — on **every**
   exit path including failure and SIGINT.

## Local: `run-local.sh`

```sh
echo "$YOUR_LINODE_PAT" > ~/.linode.pat   # Linodes: Read/Write only
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

- **Trigger**: every push, PR, tag, and manual `workflow_dispatch`.
- **Gate**: the `LINODE_E2E_TOKEN` secret existing. Without it the job skips
  with no spend — so the workflow can be merged before the secret is
  configured.
- **Required check**: added to branch-protection on `main`, so a failing
  `e2e-linode` blocks PR merge alongside `test` and `lint`. (When the secret
  isn't set, the job skips and counts as success for branch protection.)
- **Publish gate**: `publish` will not run if `e2e-linode` failed. A skip is
  fine — publish proceeds when the Linode secret isn't configured.
- **Cost per real run**: ~1¢ (one paid hour on `g6-nanode-1`). PRs that push
  multiple commits cost ~1¢ each.
- **Cleanup**: an `if: always()` step lists Linodes by the run's tag and
  destroys any survivor, complementing fj-bellows' own `-destroy-on-exit`.

### What it does NOT assert

It does NOT wait for idle teardown to fire. Linode bills whole hours rounded
up, so the orchestrator deliberately keeps the warm worker until `:55` of the
paid hour — asserting fast teardown would contradict the design. The teardown
policy is covered by the orchestrator unit tests (`internal/orchestrator/
teardown_test.go`). The E2E asserts the live behavior that's hardest to mock:
that a real Linode is provisioned, that the ephemeral REST registration works
against a v15 Forgejo, and that `forgejo-runner one-job --handle` runs to
completion.
