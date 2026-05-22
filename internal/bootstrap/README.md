# internal/bootstrap

Renders the **provider-agnostic** cloud-init that prepares a worker VM. The
template (`cloud-init.yaml.tmpl`) is embedded at build time with `//go:embed`.

The bootstrap deliberately holds **no Forgejo credentials**: it installs Docker
and the `forgejo-runner` binary, then touches a readiness sentinel
(`DefaultReadyFile`). The orchestrator registers an ephemeral runner per job and
delivers the one-shot token over SSH at dispatch time.

The install step detects the VM architecture (`uname -m`) and fetches the
matching `forgejo-runner` build, so both amd64 and arm64 workers are supported.

```go
userData, err := bootstrap.Render(bootstrap.Params{RunnerVersion: "12.10.1"})
```
