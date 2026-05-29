// Package bootstrap renders the provider-agnostic cloud-init that prepares a
// worker VM. The template is embedded at build time.
package bootstrap

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"text/template"
)

//go:embed cloud-init.yaml.tmpl
var cloudInitTemplate string

//go:embed fjbagent.service
var fjbagentServiceUnit string

// FJBAgentServiceUnit returns the embedded fjbagent systemd unit so
// other cloud-init renderers (the linode cache, etc.) can write the
// same unit without duplicating the file. Returning a copy keeps the
// embedded variable read-only to callers.
func FJBAgentServiceUnit() string { return fjbagentServiceUnit }

// DefaultReadyFile is touched by cloud-init once the worker is provisioned.
// The orchestrator polls for it over SSH to decide a node is ready.
const DefaultReadyFile = "/run/fj-bellows-ready"

// DefaultAgentListenPort is the TCP port fjbagent binds on the worker's
// VPC IP (cache binds the same port on its WG inner address). Exposed as
// a constant so the orchestrator dialer and the cloud-init renderer agree
// without an extra config knob.
const DefaultAgentListenPort = 9001

// DefaultAgentDownloadURLTemplate is the URL the orchestrator resolves
// against its own build version (via ResolveAgentDownloadURL) before
// passing the result into Params.FJBAgentDownloadURL. The agent is part
// of the same fj-bellows codebase as the orchestrator — there is no
// per-deployment choice of agent version, only a per-build one. {{.Version}}
// resolves to the orchestrator's main.version (linker-stamped); $rarch
// stays a shell literal so cloud-init substitutes it at boot from the
// worker's actual architecture.
//
// Pre-existing workers from before an orchestrator upgrade keep running
// the agent version they were provisioned with — proto-wire compatibility
// between agent versions is the contract that lets the orchestrator dial
// older workers without a forced reap.
const DefaultAgentDownloadURLTemplate = "https://github.com/hstern/fj-bellows/releases/download/v{{.Version}}/fjbagent-linux-$rarch"

// Params fills the cloud-init template.
type Params struct {
	// RunnerVersion is the forgejo-runner release to install, without a
	// leading "v" (e.g. "12.10.1").
	RunnerVersion string
	// ReadyFile is the readiness sentinel path.
	ReadyFile string
	// HostPrivateKey, when non-empty, is an OpenSSH-format PEM private key the
	// worker is configured to use as its ed25519 SSH host key. The orchestrator
	// generates it per-VM and pre-pins the matching public key so the very first
	// dial is verified, eliminating the trust-on-first-use window. When empty the
	// worker keeps the host key cloud-init generates on its own.
	HostPrivateKey string

	// FJBAgentDownloadURL is the fully-resolved URL the worker fetches the
	// fjbagent binary from. Use ResolveAgentDownloadURL to substitute the
	// orchestrator's version into a template first; $rarch stays a shell
	// literal cloud-init substitutes at boot. Empty disables agent install
	// entirely (transitional deployments).
	FJBAgentDownloadURL string

	// FJBAgentToken is the per-deployment shared-secret bearer token the
	// agent and orchestrator agree on. Written to /etc/fjbagent/auth.token
	// (mode 0600, owned by the fjb user) and presented by the orchestrator
	// in the Authorization header on every Connect RPC. Required when
	// FJBAgentDownloadURL is non-empty.
	FJBAgentToken string
}

// Render produces the cloud-init user-data for a worker.
func Render(p Params) (string, error) {
	if p.RunnerVersion == "" {
		return "", errors.New("bootstrap: RunnerVersion is required")
	}
	if p.ReadyFile == "" {
		p.ReadyFile = DefaultReadyFile
	}
	if p.FJBAgentDownloadURL != "" && p.FJBAgentToken == "" {
		return "", errors.New("bootstrap: FJBAgentToken is required when FJBAgentDownloadURL is set")
	}

	tmplData := struct {
		Params
		FJBAgentServiceUnit string
		FJBAgentListenPort  int
	}{
		Params:              p,
		FJBAgentServiceUnit: fjbagentServiceUnit,
		FJBAgentListenPort:  DefaultAgentListenPort,
	}

	funcs := template.FuncMap{
		"b64enc": func(s string) string {
			return base64.StdEncoding.EncodeToString([]byte(s))
		},
		"indent": func(spaces int, s string) string {
			prefix := strings.Repeat(" ", spaces)
			lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
			for i, line := range lines {
				lines[i] = prefix + line
			}
			return strings.Join(lines, "\n")
		},
	}
	tmpl, err := template.New("cloud-init").Funcs(funcs).Parse(cloudInitTemplate)
	if err != nil {
		return "", fmt.Errorf("parse cloud-init template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, tmplData); err != nil {
		return "", fmt.Errorf("render cloud-init: %w", err)
	}
	return buf.String(), nil
}

// ResolveAgentDownloadURL substitutes the {{.Version}} placeholder in
// urlTemplate with the orchestrator's own build version. $rarch stays a
// shell literal cloud-init substitutes at boot. The agent version is
// tied to the orchestrator's build by construction — there is no
// per-deployment version choice.
func ResolveAgentDownloadURL(urlTemplate, version string) (string, error) {
	if urlTemplate == "" {
		urlTemplate = DefaultAgentDownloadURLTemplate
	}
	tmpl, err := template.New("agent-url").Parse(urlTemplate)
	if err != nil {
		return "", fmt.Errorf("parse agent download URL: %w", err)
	}
	var buf bytes.Buffer
	// Arch stays as a shell variable ($rarch) the cloud-init replaces at
	// boot time; we only resolve the version here.
	data := struct {
		Version string
		Arch    string
	}{Version: version, Arch: "$rarch"}
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute agent download URL: %w", err)
	}
	return buf.String(), nil
}
