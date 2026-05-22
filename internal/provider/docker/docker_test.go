package docker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

const (
	testTag = "pool-x"
	testIP  = "172.17.0.2"
)

// fakeAPI is an in-memory dockerAPI for tests: no daemon required.
type fakeAPI struct {
	created    *container.Config
	createName string
	started    []string
	removed    []string
	removedF   []bool
	listArgs   filters.Args
	listResult []container.Summary
	inspectIP  string
}

func (f *fakeAPI) ContainerCreate(_ context.Context, config *container.Config, _ *container.HostConfig, name string) (string, error) {
	f.created = config
	f.createName = name
	return "container123", nil
}

func (f *fakeAPI) ContainerStart(_ context.Context, id string) error {
	f.started = append(f.started, id)
	return nil
}

func (f *fakeAPI) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: id, Name: "/worker-1", Created: "2026-01-02T03:04:05Z"},
		Config:            &container.Config{Labels: map[string]string{tagLabel: testTag}},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"bridge": {IPAddress: f.inspectIP},
			},
		},
	}, nil
}

func (f *fakeAPI) ContainerRemove(_ context.Context, id string, force bool) error {
	f.removed = append(f.removed, id)
	f.removedF = append(f.removedF, force)
	return nil
}

func (f *fakeAPI) ContainerList(_ context.Context, args filters.Args) ([]container.Summary, error) {
	f.listArgs = args
	return f.listResult, nil
}

// okDial always reports the address as reachable.
func okDial(_ context.Context, _ string) error { return nil }

func newTestDocker(api dockerAPI) *Docker {
	return &Docker{
		cfg:  config{Image: "example/worker:latest", SSHPort: defaultSSHPort, SSHReadyTimeout: 5 * time.Second},
		api:  api,
		dial: okDial,
	}
}

func nodeFromYAML(t *testing.T, s string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(s), &n); err != nil {
		t.Fatal(err)
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return *n.Content[0]
	}
	return n
}

func TestConfigureMissingImage(t *testing.T) {
	d := &Docker{}
	if err := d.Configure(nodeFromYAML(t, `ssh_port: 22`)); err == nil {
		t.Fatal("expected error for missing image")
	}
}

func TestConfigureDefaults(t *testing.T) {
	d := &Docker{}
	if err := d.Configure(nodeFromYAML(t, `image: example/worker:latest`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if d.cfg.SSHPort != defaultSSHPort {
		t.Errorf("SSHPort default = %d", d.cfg.SSHPort)
	}
	if d.cfg.SSHReadyTimeout == 0 {
		t.Error("SSHReadyTimeout default not set")
	}
}

func TestProvisionStampsTagAndInjectsKey(t *testing.T) {
	api := &fakeAPI{inspectIP: testIP}
	d := newTestDocker(api)
	inst, err := d.Provision(context.Background(), provider.Spec{
		Tag:           testTag,
		Name:          "worker-1",
		AuthorizedKey: "ssh-ed25519 AAAAKEY orchestrator",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Tag stamped as a label.
	if got := api.created.Labels[tagLabel]; got != testTag {
		t.Errorf("label %s = %q, want pool-x", tagLabel, got)
	}
	// Key injected via env.
	wantEnv := authorizedKeyEnv + "=ssh-ed25519 AAAAKEY orchestrator"
	if len(api.created.Env) != 1 || api.created.Env[0] != wantEnv {
		t.Errorf("Env = %v, want [%q]", api.created.Env, wantEnv)
	}
	// Image and name forwarded; container started.
	if api.created.Image != "example/worker:latest" {
		t.Errorf("Image = %q", api.created.Image)
	}
	if api.createName != "worker-1" {
		t.Errorf("name = %q", api.createName)
	}
	if len(api.started) != 1 || api.started[0] != "container123" {
		t.Errorf("started = %v", api.started)
	}
	// Returned instance carries id, ip, tag.
	if inst.ID != "container123" || inst.IPv4 != testIP || inst.Tag != testTag {
		t.Errorf("instance = %+v", inst)
	}
	if inst.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

func TestProvisionWithoutKeyOmitsEnv(t *testing.T) {
	api := &fakeAPI{inspectIP: testIP}
	d := newTestDocker(api)
	if _, err := d.Provision(context.Background(), provider.Spec{Tag: "t", Name: "n"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(api.created.Env) != 0 {
		t.Errorf("Env = %v, want empty", api.created.Env)
	}
}

func TestProvisionSSHNotReady(t *testing.T) {
	api := &fakeAPI{inspectIP: testIP}
	d := newTestDocker(api)
	d.cfg.SSHReadyTimeout = 100 * time.Millisecond
	d.dial = func(_ context.Context, _ string) error { return errors.New("connection refused") }
	if _, err := d.Provision(context.Background(), provider.Spec{Tag: "t", Name: "n"}); err == nil {
		t.Fatal("expected ssh-not-ready error")
	}
}

func TestListFiltersByTag(t *testing.T) {
	api := &fakeAPI{
		listResult: []container.Summary{
			{
				ID:      "abc",
				Names:   []string{"/worker-1"},
				Labels:  map[string]string{tagLabel: testTag},
				Created: 1735787045,
				NetworkSettings: &container.NetworkSettingsSummary{
					Networks: map[string]*network.EndpointSettings{
						"bridge": {IPAddress: "172.17.0.3"},
					},
				},
			},
		},
	}
	d := newTestDocker(api)
	out, err := d.List(context.Background(), testTag)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// The label filter must be passed to the daemon.
	if !api.listArgs.ExactMatch("label", tagLabel+"="+testTag) {
		t.Errorf("list filter args = %v", api.listArgs)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d", len(out))
	}
	got := out[0]
	if got.ID != "abc" || got.Name != "worker-1" || got.IPv4 != "172.17.0.3" || got.Tag != testTag {
		t.Errorf("instance = %+v", got)
	}
	if got.CreatedAt.Unix() != 1735787045 {
		t.Errorf("CreatedAt = %v", got.CreatedAt)
	}
}

func TestDestroyForceRemoves(t *testing.T) {
	api := &fakeAPI{}
	d := newTestDocker(api)
	if err := d.Destroy(context.Background(), "xyz"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(api.removed) != 1 || api.removed[0] != "xyz" {
		t.Errorf("removed = %v", api.removed)
	}
	if len(api.removedF) != 1 || !api.removedF[0] {
		t.Errorf("force = %v, want true", api.removedF)
	}
}

func TestBillingModel(t *testing.T) {
	d := &Docker{}
	if d.BillingModel() != provider.BillingPerSecond {
		t.Errorf("BillingModel = %v", d.BillingModel())
	}
}

func TestRegisteredInRegistry(t *testing.T) {
	p, err := provider.New("docker")
	if err != nil {
		t.Fatalf("docker not registered: %v", err)
	}
	if _, ok := p.(*Docker); !ok {
		t.Errorf("registry returned %T", p)
	}
}
