package linode

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"text/template"
)

// wrapWorkerUserDataForCache returns a multipart/mixed MIME body that
// cloud-init merges into one effective config. Part 1 is the base
// worker user-data the orchestrator rendered (unchanged); part 2 is
// the cache-extras fragment that adds CA trust, /etc/hosts mapping
// for the cache hostname, and a containerd hosts.toml with PULL-ONLY
// capabilities (the boundary that keeps `docker push <upstream>`
// going direct to upstream instead of being captured by the cache).
//
// cloud-init's merge semantics handle the combination natively when
// each part declares Content-Type: text/cloud-config — write_files
// and runcmd entries from both parts are concatenated.
func wrapWorkerUserDataForCache(baseUserData string, x workerExtrasData) (string, error) {
	if baseUserData == "" {
		return "", errors.New("wrapWorkerUserDataForCache: base user-data is empty")
	}
	extras, err := renderWorkerCacheExtras(x)
	if err != nil {
		return "", fmt.Errorf("render worker cache extras: %w", err)
	}
	return multipartCloudInit([]string{baseUserData, extras}), nil
}

// multipartCloudInit returns the MIME multipart wrapper cloud-init
// recognises. Each part is written with Content-Type:
// text/cloud-config so cloud-init merges them.
//
// Boundary is a fixed string; the parts must not contain it. Since
// the parts are YAML cloud-configs we render, the boundary is unique
// enough that a collision is implausible. (If a part ever does
// contain it we'd switch to a random boundary, but the current
// renderer is deterministic and the constant boundary keeps the
// rendered user-data byte-stable for tests.)
const cloudInitBoundary = "==fjb-cache-boundary=="

func multipartCloudInit(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	var buf strings.Builder
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", cloudInitBoundary)
	for _, p := range parts {
		fmt.Fprintf(&buf, "--%s\r\n", cloudInitBoundary)
		fmt.Fprintf(&buf, "Content-Type: text/cloud-config\r\n")
		fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n\r\n")
		// RFC 2046 §5.1.1: boundaries are preceded by CRLF. Normalize
		// the part body to end with exactly one CRLF, then write the
		// next boundary on the following line.
		buf.WriteString(strings.TrimRight(p, "\r\n"))
		buf.WriteString("\r\n")
	}
	fmt.Fprintf(&buf, "--%s--\r\n", cloudInitBoundary)
	return buf.String()
}

//go:embed worker-cache-extras.yaml.tmpl
var workerCacheExtrasTemplate string

// renderWorkerCacheExtras fills the worker-side cache cloud-init
// fragment. Required fields are guarded — an empty CA PEM or cache IP
// would silently produce broken trust / unreachable cache, so we
// refuse to render in that state.
func renderWorkerCacheExtras(x workerExtrasData) (string, error) {
	if err := validateWorkerExtras(x); err != nil {
		return "", err
	}
	tmpl, err := template.New("worker-cache-extras").Funcs(template.FuncMap{
		"indent": func(spaces int, s string) string {
			prefix := strings.Repeat(" ", spaces)
			lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
			for i, line := range lines {
				lines[i] = prefix + line
			}
			return strings.Join(lines, "\n")
		},
	}).Parse(workerCacheExtrasTemplate)
	if err != nil {
		return "", fmt.Errorf("parse worker cache extras template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, x); err != nil {
		return "", fmt.Errorf("execute worker cache extras template: %w", err)
	}
	return buf.String(), nil
}

// validateWorkerExtras asserts the inputs to the worker cache
// fragment are all populated. Kept separate from the renderer to
// keep the renderer's complexity low and so callers can surface a
// more useful error message than "template execution failed".
func validateWorkerExtras(x workerExtrasData) error {
	missing := []string{}
	if x.CACertPEM == "" {
		missing = append(missing, "CACertPEM")
	}
	if x.CacheHost == "" {
		missing = append(missing, "CacheHost")
	}
	if x.CacheIP == "" {
		missing = append(missing, "CacheIP")
	}
	if x.CachePort == 0 {
		missing = append(missing, "CachePort")
	}
	if x.UpstreamHost == "" {
		missing = append(missing, "UpstreamHost")
	}
	if len(missing) > 0 {
		return fmt.Errorf("worker cache extras: missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}
