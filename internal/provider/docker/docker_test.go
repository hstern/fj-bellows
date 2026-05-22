package docker

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

const testTag = "pool-x"

func newTestDocker(api dockerAPI) *Docker {
	return &Docker{
		cfg: config{Image: "example/worker:latest"},
		api: api,
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
	d := &Docker{api: &fakeAPI{}}
	if err := d.Configure(nodeFromYAML(t, `network: bridge`)); err == nil {
		t.Fatal("expected error for missing image")
	}
}

func TestConfigureDecodesImageAndNetwork(t *testing.T) {
	d := &Docker{api: &fakeAPI{}}
	if err := d.Configure(nodeFromYAML(t, "image: example/worker:latest\nnetwork: ci-net")); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if d.cfg.Image != "example/worker:latest" {
		t.Errorf("Image = %q", d.cfg.Image)
	}
	if d.cfg.Network != "ci-net" {
		t.Errorf("Network = %q", d.cfg.Network)
	}
}

func TestProvisionStampsTagNoKeyNoPort(t *testing.T) {
	api := &fakeAPI{}
	d := newTestDocker(api)
	d.cfg.Network = "ci-net"
	inst, err := d.Provision(context.Background(), provider.Spec{
		Tag:           testTag,
		Name:          "worker-1",
		AuthorizedKey: "ssh-ed25519 AAAAKEY orchestrator",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	assertProvision(t, api, inst)
}

func assertProvision(t *testing.T, api *fakeAPI, inst provider.Instance) {
	t.Helper()
	assertCreateConfig(t, api)
	assertNoSSHBaggage(t, api)
	assertReturnedInstance(t, inst)
}

func assertCreateConfig(t *testing.T, api *fakeAPI) {
	t.Helper()
	if got := api.created.Labels[tagLabel]; got != testTag {
		t.Errorf("label %s = %q, want %s", tagLabel, got, testTag)
	}
	if api.created.Image != "example/worker:latest" {
		t.Errorf("Image = %q", api.created.Image)
	}
	if api.createName != "worker-1" {
		t.Errorf("name = %q", api.createName)
	}
	if len(api.started) != 1 || api.started[0] != "container123" {
		t.Errorf("started = %v", api.started)
	}
	if api.hostCfg == nil || string(api.hostCfg.NetworkMode) != "ci-net" {
		t.Errorf("NetworkMode = %v", api.hostCfg)
	}
}

func assertNoSSHBaggage(t *testing.T, api *fakeAPI) {
	t.Helper()
	if len(api.created.Env) != 0 {
		t.Errorf("Env = %v, want empty (no key injection)", api.created.Env)
	}
	if len(api.created.ExposedPorts) != 0 {
		t.Errorf("ExposedPorts = %v, want none", api.created.ExposedPorts)
	}
	if api.hostCfg != nil && len(api.hostCfg.PortBindings) != 0 {
		t.Errorf("PortBindings = %v, want none", api.hostCfg.PortBindings)
	}
}

func assertReturnedInstance(t *testing.T, inst provider.Instance) {
	t.Helper()
	if inst.ID != "container123" || inst.Tag != testTag || inst.IPv4 != "" {
		t.Errorf("instance = %+v", inst)
	}
	if inst.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
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
	if got.ID != "abc" || got.Name != "worker-1" || got.Tag != testTag {
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
