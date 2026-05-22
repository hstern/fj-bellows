# cmd/fj-bellows

The daemon entrypoint. It:

1. Loads config (`-config`).
2. Acquires the singleton advisory lock (`-lock`) so only one daemon makes
   provisioning decisions.
3. Reads the SSH key file (deriving the public key to inject into workers). The
   Forgejo and provider tokens are inline in config.yaml.
4. Constructs the configured provider and hands it the opaque `provider_config`.
5. Builds the Forgejo client and the SSH dispatcher.
6. Runs the orchestrator until SIGINT/SIGTERM.

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-config` | `/etc/fj-bellows/config.yaml` | config file path |
| `-lock` | `/run/fj-bellows.lock` | singleton lock file |
| `-runner-version` | `12.10.1` | forgejo-runner version installed on workers |

In-tree providers are registered via blank imports here; add a new provider by
importing its package.
