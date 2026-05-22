// Package bootstrap renders the provider-agnostic cloud-init that prepares a
// worker VM. The template is embedded at build time.
package bootstrap

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"text/template"
)

//go:embed cloud-init.yaml.tmpl
var cloudInitTemplate string

// DefaultReadyFile is touched by cloud-init once the worker is provisioned.
// The orchestrator polls for it over SSH to decide a node is ready.
const DefaultReadyFile = "/run/fj-bellows-ready"

// Params fills the cloud-init template.
type Params struct {
	// RunnerVersion is the forgejo-runner release to install, without a
	// leading "v" (e.g. "12.10.1").
	RunnerVersion string
	// ReadyFile is the readiness sentinel path.
	ReadyFile string
}

// Render produces the cloud-init user-data for a worker.
func Render(p Params) (string, error) {
	if p.RunnerVersion == "" {
		return "", errors.New("bootstrap: RunnerVersion is required")
	}
	if p.ReadyFile == "" {
		p.ReadyFile = DefaultReadyFile
	}
	tmpl, err := template.New("cloud-init").Parse(cloudInitTemplate)
	if err != nil {
		return "", fmt.Errorf("parse cloud-init template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		return "", fmt.Errorf("render cloud-init: %w", err)
	}
	return buf.String(), nil
}
