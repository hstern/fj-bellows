package docker

import (
	"context"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// dockerAPI is the subset of the Docker client this package uses. Abstracting
// it lets unit tests substitute a fake so they need no running daemon.
type dockerAPI interface {
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, name string) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
	ContainerRemove(ctx context.Context, id string, force bool) error
	ContainerList(ctx context.Context, args filters.Args) ([]container.Summary, error)

	// Exec surface used by the docker-exec dispatcher.
	ContainerExecCreate(ctx context.Context, id string, opts container.ExecOptions) (string, error)
	ContainerExecAttach(ctx context.Context, execID string) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
}

// realClient adapts the concrete Docker client to the unexported dockerAPI
// interface, narrowing its surface and dropping arguments this package does
// not use.
type realClient struct {
	cli *client.Client
}

func (r realClient) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, name string) (string, error) {
	resp, err := r.cli.ContainerCreate(ctx, config, hostConfig, nil, nil, name)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (r realClient) ContainerStart(ctx context.Context, id string) error {
	return r.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (r realClient) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	return r.cli.ContainerInspect(ctx, id)
}

func (r realClient) ContainerRemove(ctx context.Context, id string, force bool) error {
	return r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force})
}

func (r realClient) ContainerList(ctx context.Context, args filters.Args) ([]container.Summary, error) {
	return r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
}

func (r realClient) ContainerExecCreate(ctx context.Context, id string, opts container.ExecOptions) (string, error) {
	resp, err := r.cli.ContainerExecCreate(ctx, id, opts)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (r realClient) ContainerExecAttach(ctx context.Context, execID string) (types.HijackedResponse, error) {
	return r.cli.ContainerExecAttach(ctx, execID, container.ExecAttachOptions{})
}

func (r realClient) ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error) {
	return r.cli.ContainerExecInspect(ctx, execID)
}
