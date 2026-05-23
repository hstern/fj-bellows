// Package docker implements provider.Provider on top of the local docker CLI.
//
// Containers are created with a label fj-bellows.tag=<spec.Tag>, which is the
// sole basis on which the orchestrator List/Destroy/reconcile owns them. The
// provider bills per-second (containers are local), so the core uses a plain
// idle timeout for teardown.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

// tagLabel is the docker label that marks a container as owned by an
// fj-bellows deployment with the given tag.
const tagLabel = "fj-bellows.tag"

// defaultDockerBin is the binary name resolved on PATH when provider_config
// does not override it.
const defaultDockerBin = "docker"

// defaultWaitTimeout bounds how long WaitReady polls for a container to enter
// the Running state.
const defaultWaitTimeout = 30 * time.Second

// flagLabel is the docker CLI flag that sets a container label / filter.
// Centralized so goconst stays quiet and so a future flag rename happens once.
const flagLabel = "--label"

// config is the provider_config subtree for docker.
type config struct {
	// Image is the worker image to run. It must contain forgejo-runner on
	// PATH and a long-running entrypoint (so docker exec can land in it).
	Image string `yaml:"image"`

	// Network optionally attaches the container to a named docker network.
	Network string `yaml:"network"`

	// Volumes are docker -v bind mounts applied to every worker container,
	// each formatted "<host>:<container>[:<mode>]". A typical use is mounting
	// the host's Docker socket so forgejo-runner can spawn step containers on
	// the host daemon without nested Docker.
	Volumes []string `yaml:"volumes"`

	// DockerBin overrides the docker binary name (default "docker"). Useful
	// when docker is installed under a non-standard name (podman shim, etc.).
	DockerBin string `yaml:"docker_bin"`

	// WaitTimeout bounds how long the dispatcher polls for Running state.
	WaitTimeout Duration `yaml:"wait_timeout"`
}

// Duration is a yaml-decodable time.Duration accepting "30s", "2m", etc.
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Docker is the provider implementation.
type Docker struct {
	cfg config
	cli cli
}

func init() {
	provider.Register("docker", func() provider.Provider { return &Docker{} })
}

// Configure decodes provider_config, applies defaults, validates, and
// prepares the docker CLI client.
func (d *Docker) Configure(node yaml.Node) error {
	if err := node.Decode(&d.cfg); err != nil {
		return fmt.Errorf("docker: decode provider_config: %w", err)
	}
	if d.cfg.Image == "" {
		return errors.New("docker: provider_config missing: image")
	}
	if d.cfg.DockerBin == "" {
		d.cfg.DockerBin = defaultDockerBin
	}
	if d.cfg.WaitTimeout == 0 {
		d.cfg.WaitTimeout = Duration(defaultWaitTimeout)
	}
	d.cli = newExecCLI(d.cfg.DockerBin)
	return nil
}

// DockerBin returns the resolved docker binary (defaulted in Configure).
// Exposed for the composition root to wire the matching exec dispatcher.
func (d *Docker) DockerBin() string { return d.cfg.DockerBin }

// WaitTimeout returns the resolved wait timeout (defaulted in Configure).
// Exposed for the composition root to wire the matching exec dispatcher.
func (d *Docker) WaitTimeout() time.Duration { return d.cfg.WaitTimeout.D() }

// Provision creates a detached container tagged with spec.Tag and returns its
// container ID. IPv4 is left empty: the docker-exec dispatcher addresses the
// container by ID, not network address.
func (d *Docker) Provision(ctx context.Context, spec provider.Spec) (provider.Instance, error) {
	args := []string{"run", "-d", flagLabel, tagLabel + "=" + spec.Tag}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	if d.cfg.Network != "" {
		args = append(args, "--network", d.cfg.Network)
	}
	for _, v := range d.cfg.Volumes {
		args = append(args, "-v", v)
	}
	args = append(args, d.cfg.Image)
	out, err := d.cli.Run(ctx, nil, args...)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("docker: run: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return provider.Instance{}, errors.New("docker: run returned empty container id")
	}
	return provider.Instance{
		ID:        id,
		Name:      spec.Name,
		CreatedAt: time.Now().UTC(),
		Tag:       spec.Tag,
	}, nil
}

// Destroy force-removes the container with the given ID.
func (d *Docker) Destroy(ctx context.Context, id string) error {
	if _, err := d.cli.Run(ctx, nil, "rm", "-f", id); err != nil {
		return fmt.Errorf("docker: rm: %w", err)
	}
	return nil
}

// List returns all containers (running or not) carrying the fj-bellows.tag
// label equal to tag.
func (d *Docker) List(ctx context.Context, tag string) ([]provider.Instance, error) {
	// Use a custom Go-template format so we get an unambiguous, RFC3339-ish
	// timestamp instead of docker ps's locale-dependent default.
	out, err := d.cli.Run(
		ctx, nil,
		"ps", "-a",
		"--filter", "label="+tagLabel+"="+tag,
		"--format", "{{.ID}}\t{{.Names}}\t{{.CreatedAt}}",
	)
	if err != nil {
		return nil, fmt.Errorf("docker: ps: %w", err)
	}
	return parsePSOutput(out, tag)
}

// parsePSOutput parses the tab-separated docker ps --format output. Lines
// with an unparseable timestamp keep a zero CreatedAt rather than failing the
// whole list — reconcile tolerates a zero time and recomputes the teardown
// timer next tick.
func parsePSOutput(out []byte, tag string) ([]provider.Instance, error) {
	var instances []provider.Instance
	for line := range bytes.SplitSeq(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		fields := strings.SplitN(string(line), "\t", 3)
		if len(fields) < 2 {
			continue
		}
		inst := provider.Instance{
			ID:   fields[0],
			Name: fields[1],
			Tag:  tag,
		}
		if len(fields) == 3 {
			inst.CreatedAt = parseDockerTime(fields[2])
		}
		instances = append(instances, inst)
	}
	return instances, nil
}

// parseDockerTime parses the timestamps docker prints for {{.CreatedAt}},
// which look like "2026-05-22 12:34:56 +0000 UTC". Returns a zero time on
// failure; callers treat that as "unknown, recompute next tick".
func parseDockerTime(s string) time.Time {
	s = strings.TrimSpace(s)
	// docker's CreatedAt format includes a trailing zone name we strip.
	// Try a couple of layouts that cover docker's known outputs.
	layouts := []string{
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 -0700",
		time.RFC3339,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// BillingModel reports per-second billing: containers are local, there is no
// hourly rounding to amortize over.
func (d *Docker) BillingModel() provider.BillingModel {
	return provider.BillingPerSecond
}
