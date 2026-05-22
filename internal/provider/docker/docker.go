// Package docker implements the provider.Provider interface backed by local
// Docker containers, plus a docker-exec dispatcher (see exec.go) that drives
// those containers without SSH. It provisions real containers running a
// forgejo-runner, giving zero-cost real provisioning for development and
// integration tests without any cloud account or SSH key.
//
// Containers bill nothing, so this provider reports BillingPerSecond and the
// core uses a plain idle timeout. Every container is stamped with the pool tag
// as a label (fj-bellows.tag=<tag>) so List, reconcile, and the orphan sweep
// can find them.
//
// Unlike SSH-based providers, the docker provider neither injects an
// authorized key, exposes a port, nor runs sshd: the orchestrator reaches the
// worker via `docker exec` over the local Docker socket. See the package README
// for the full worker-image contract.
package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

// tagLabel is the container label that carries the pool tag.
const tagLabel = "fj-bellows.tag"

// config is the provider_config subtree for the Docker provider.
type config struct {
	// Image is the worker image (ships forgejo-runner). Required.
	Image string `yaml:"image"`
	// Network is an optional Docker network to attach the container to.
	Network string `yaml:"network"`
}

// Docker is the provider implementation.
type Docker struct {
	cfg config
	api dockerAPI
}

func init() {
	provider.Register("docker", func() provider.Provider { return &Docker{} })
}

// Configure decodes the opaque node, validates required fields, and prepares
// the Docker client.
func (d *Docker) Configure(node yaml.Node) error {
	if err := node.Decode(&d.cfg); err != nil {
		return fmt.Errorf("docker: decode provider_config: %w", err)
	}
	if strings.TrimSpace(d.cfg.Image) == "" {
		return errors.New("docker: provider_config missing: image")
	}
	if d.api == nil {
		api, err := newRealClient()
		if err != nil {
			return err
		}
		d.api = api
	}
	return nil
}

// Provision creates and starts a container from the configured image and stamps
// the pool tag as a label. It injects no key and exposes no port: the worker is
// reached via `docker exec`, so the returned Instance has an empty IPv4.
func (d *Docker) Provision(ctx context.Context, spec provider.Spec) (provider.Instance, error) {
	cfg := &container.Config{
		Image:  d.cfg.Image,
		Labels: map[string]string{tagLabel: spec.Tag},
	}
	hostCfg := &container.HostConfig{}
	if d.cfg.Network != "" {
		hostCfg.NetworkMode = container.NetworkMode(d.cfg.Network)
	}
	id, err := d.api.ContainerCreate(ctx, cfg, hostCfg, spec.Name)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("docker: create container: %w", err)
	}
	if err := d.api.ContainerStart(ctx, id); err != nil {
		return provider.Instance{}, fmt.Errorf("docker: start container %s: %w", id, err)
	}
	return provider.Instance{
		ID:        id,
		IPv4:      "", // unused: dispatch is via docker exec, not the network.
		CreatedAt: time.Now(),
		Tag:       spec.Tag,
	}, nil
}

// Destroy force-removes the container with the given ID.
func (d *Docker) Destroy(ctx context.Context, id string) error {
	if err := d.api.ContainerRemove(ctx, id, true); err != nil {
		return fmt.Errorf("docker: remove container %s: %w", id, err)
	}
	return nil
}

// List returns all containers carrying tag, matched by the tag label.
func (d *Docker) List(ctx context.Context, tag string) ([]provider.Instance, error) {
	args := filters.NewArgs(filters.Arg("label", tagLabel+"="+tag))
	summaries, err := d.api.ContainerList(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("docker: list containers: %w", err)
	}
	out := make([]provider.Instance, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, summaryToInstance(s))
	}
	return out, nil
}

// BillingModel reports per-second billing: containers are free, so the core
// uses a plain idle timeout with no warm-hold.
func (d *Docker) BillingModel() provider.BillingModel {
	return provider.BillingPerSecond
}

// summaryToInstance maps a list summary to a provider.Instance.
func summaryToInstance(s container.Summary) provider.Instance {
	var name string
	if len(s.Names) > 0 {
		name = strings.TrimPrefix(s.Names[0], "/")
	}
	return provider.Instance{
		ID:        s.ID,
		Name:      name,
		IPv4:      "", // unused for docker-exec dispatch.
		CreatedAt: time.Unix(s.Created, 0),
		Tag:       s.Labels[tagLabel],
	}
}

// newRealClient builds a realClient from the standard Docker environment.
func newRealClient() (realClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return realClient{}, fmt.Errorf("docker: new client: %w", err)
	}
	return realClient{cli}, nil
}
