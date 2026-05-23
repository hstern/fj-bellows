# test/e2e-linode

End-to-end test for the **Linode provider** — provisions a real Linode VM via
fj-bellows, runs an ephemeral `one-job` against a local Forgejo over an SSH
reverse tunnel, then tears the VM down. Distinct from `test/integration`, which
exercises the **docker** provider in CI.

## How it works

1. A Forgejo v15 service container runs locally (or in CI), published on
   `127.0.0.1:3000` and seeded by `test/integration/seed.sh` with an admin,
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

The CI version lives in `.github/workflows/ci.yml`. It is **gated solely on the
`LINODE_E2E_TOKEN` secret** — runs on every push/PR/tag (and manual
`workflow_dispatch`) when the secret is set, skips entirely when it isn't, so
it costs nothing until you configure it. It is **never** added to
branch-protection required checks (PRs should not be blocked on a real-money
spend).
