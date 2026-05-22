package docker

import (
	"context"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// realClient adapts the concrete Docker client to the unexported dockerAPI
// interface, narrowing its surface and dropping arguments this provider does
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
