// Package docker implements the provider.Provider interface backed by local
// Docker containers. It provisions real containers running an sshd plus a
// forgejo-runner, giving zero-cost real provisioning and SSH for development
// and integration tests without any cloud account.
//
// Containers bill nothing, so this provider reports BillingPerSecond and the
// core uses a plain idle timeout. Every container is stamped with the pool tag
// as a label (fj-bellows.tag=<tag>) so List, reconcile, and the orphan sweep
// can find them.
//
// # Authorized-key injection contract
//
// The orchestrator's SSH public key is passed to the container through the
// FJB_AUTHORIZED_KEY environment variable. The worker image's entrypoint must
// write that value to the runner user's ~/.ssh/authorized_keys before starting
// sshd, so the orchestrator can connect with its key. See the package README
// for the full worker-image contract.
package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
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

// authorizedKeyEnv is the environment variable the worker image's entrypoint
// reads to populate the runner's authorized_keys.
const authorizedKeyEnv = "FJB_AUTHORIZED_KEY"

// defaultSSHPort is the in-container port sshd listens on.
const defaultSSHPort = 22

// dockerAPI is the subset of the Docker client this provider uses. Abstracting
// it lets unit tests substitute a fake so they need no running daemon.
type dockerAPI interface {
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, name string) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
	ContainerRemove(ctx context.Context, id string, force bool) error
	ContainerList(ctx context.Context, args filters.Args) ([]container.Summary, error)
}

// dialer reports whether a host:port accepts TCP connections. Abstracting it
// lets tests assert SSH-readiness behaviour without opening real sockets.
type dialer func(ctx context.Context, addr string) error

// config is the provider_config subtree for the Docker provider.
type config struct {
	// Image is the worker image (sshd + forgejo-runner). Required.
	Image string `yaml:"image"`
	// SSHPort is the in-container sshd port to probe. Defaults to 22.
	SSHPort int `yaml:"ssh_port"`
	// SSHReadyTimeout bounds the wait for sshd to accept connections.
	// Defaults to 30s.
	SSHReadyTimeout time.Duration `yaml:"ssh_ready_timeout"`
}

// Docker is the provider implementation.
type Docker struct {
	cfg  config
	api  dockerAPI
	dial dialer
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
	if d.cfg.SSHPort == 0 {
		d.cfg.SSHPort = defaultSSHPort
	}
	if d.cfg.SSHReadyTimeout == 0 {
		d.cfg.SSHReadyTimeout = 30 * time.Second
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker: new client: %w", err)
	}
	d.api = realClient{cli}
	if d.dial == nil {
		d.dial = tcpDialer
	}
	return nil
}

// Provision creates and starts a container from the configured image, stamps
// the pool tag as a label, injects the orchestrator key via the environment,
// and waits for sshd to accept connections before returning.
func (d *Docker) Provision(ctx context.Context, spec provider.Spec) (provider.Instance, error) {
	cfg := &container.Config{
		Image:  d.cfg.Image,
		Labels: map[string]string{tagLabel: spec.Tag},
	}
	if key := strings.TrimSpace(spec.AuthorizedKey); key != "" {
		cfg.Env = append(cfg.Env, authorizedKeyEnv+"="+key)
	}
	id, err := d.api.ContainerCreate(ctx, cfg, &container.HostConfig{}, spec.Name)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("docker: create container: %w", err)
	}
	if err := d.api.ContainerStart(ctx, id); err != nil {
		return provider.Instance{}, fmt.Errorf("docker: start container %s: %w", id, err)
	}
	inst, err := d.inspect(ctx, id)
	if err != nil {
		return provider.Instance{}, err
	}
	if err := d.waitSSH(ctx, inst.IPv4); err != nil {
		return provider.Instance{}, fmt.Errorf("docker: container %s ssh not ready: %w", id, err)
	}
	return inst, nil
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

// inspect resolves a started container's address and metadata.
func (d *Docker) inspect(ctx context.Context, id string) (provider.Instance, error) {
	resp, err := d.api.ContainerInspect(ctx, id)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("docker: inspect container %s: %w", id, err)
	}
	inst := provider.Instance{ID: id, IPv4: containerIP(resp)}
	if resp.ContainerJSONBase != nil {
		inst.Name = strings.TrimPrefix(resp.Name, "/")
		if t, perr := time.Parse(time.RFC3339Nano, resp.Created); perr == nil {
			inst.CreatedAt = t
		}
	}
	if resp.Config != nil {
		inst.Tag = resp.Config.Labels[tagLabel]
	}
	if inst.CreatedAt.IsZero() {
		inst.CreatedAt = time.Now()
	}
	return inst, nil
}

// waitSSH polls until sshd accepts a connection or the deadline elapses.
func (d *Docker) waitSSH(ctx context.Context, ip string) error {
	if ip == "" {
		return errors.New("container has no reachable IPv4 address")
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(d.cfg.SSHPort))
	deadline := time.Now().Add(d.cfg.SSHReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if lastErr = d.dial(ctx, addr); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("timed out")
	}
	return lastErr
}

// containerIP returns the first non-empty network IPv4 address for an inspect
// response, preferring the per-network address over the deprecated top-level
// field.
func containerIP(resp container.InspectResponse) string {
	if resp.NetworkSettings == nil {
		return ""
	}
	for _, ep := range resp.NetworkSettings.Networks {
		if ep != nil && ep.IPAddress != "" {
			return ep.IPAddress
		}
	}
	return ""
}

// summaryToInstance maps a list summary to a provider.Instance.
func summaryToInstance(s container.Summary) provider.Instance {
	var ip string
	if s.NetworkSettings != nil {
		for _, ep := range s.NetworkSettings.Networks {
			if ep != nil && ep.IPAddress != "" {
				ip = ep.IPAddress
				break
			}
		}
	}
	var name string
	if len(s.Names) > 0 {
		name = strings.TrimPrefix(s.Names[0], "/")
	}
	return provider.Instance{
		ID:        s.ID,
		Name:      name,
		IPv4:      ip,
		CreatedAt: time.Unix(s.Created, 0),
		Tag:       s.Labels[tagLabel],
	}
}

// tcpDialer is the production dialer: a short-timeout TCP connect.
func tcpDialer(ctx context.Context, addr string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}
