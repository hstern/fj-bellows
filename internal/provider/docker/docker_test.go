package docker

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

// fakeCall records one invocation of the fake cli for assertions.
type fakeCall struct {
	args  []string
	stdin string
}

// fakeCLI is an in-memory implementation of the cli interface. It serves a
// queued list of responses (stdout/err) in order, recording the args and
// stdin every call sees. Concurrency-safe so tests can race-check.
type fakeCLI struct {
	mu        sync.Mutex
	responses []fakeResponse
	calls     []fakeCall
	// onCall, when non-nil, replaces the queued-response behaviour: it is
	// invoked for every Run and may return per-call dynamic responses (used
	// by the WaitReady polling test).
	onCall func(args []string) ([]byte, error)
}

type fakeResponse struct {
	stdout []byte
	err    error
}

func (f *fakeCLI) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var in string
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		in = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{args: append([]string(nil), args...), stdin: in})
	if f.onCall != nil {
		fn := f.onCall
		f.mu.Unlock()
		return fn(args)
	}
	if len(f.responses) == 0 {
		f.mu.Unlock()
		return nil, errors.New("fakeCLI: unexpected call: " + strings.Join(args, " "))
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	f.mu.Unlock()
	return resp.stdout, resp.err
}

func (f *fakeCLI) snapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func nodeFromYAML(t *testing.T, s string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(s), &n); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return *n.Content[0]
	}
	return n
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

func TestConfigureDefaults(t *testing.T) {
	d := &Docker{}
	node := nodeFromYAML(t, `image: example/worker:latest`)
	if err := d.Configure(node); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if d.DockerBin() != "docker" {
		t.Errorf("DockerBin = %q, want docker", d.DockerBin())
	}
	if d.WaitTimeout() != 30*time.Second {
		t.Errorf("WaitTimeout = %s, want 30s", d.WaitTimeout())
	}
	if d.cli == nil {
		t.Error("Configure should construct cli")
	}
}

func TestConfigureMissingImage(t *testing.T) {
	d := &Docker{}
	node := nodeFromYAML(t, `network: foo`)
	if err := d.Configure(node); err == nil {
		t.Fatal("expected error for missing image")
	}
}

func TestConfigureOverrides(t *testing.T) {
	d := &Docker{}
	node := nodeFromYAML(t, `
image: example/worker:latest
network: my-net
docker_bin: /usr/local/bin/docker
wait_timeout: 5s
`)
	if err := d.Configure(node); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if d.DockerBin() != "/usr/local/bin/docker" {
		t.Errorf("DockerBin = %q", d.DockerBin())
	}
	if d.WaitTimeout() != 5*time.Second {
		t.Errorf("WaitTimeout = %s", d.WaitTimeout())
	}
	if d.cfg.Network != "my-net" {
		t.Errorf("Network = %q", d.cfg.Network)
	}
}

func TestConfigureBadDuration(t *testing.T) {
	d := &Docker{}
	node := nodeFromYAML(t, `
image: example/worker:latest
wait_timeout: not-a-duration
`)
	if err := d.Configure(node); err == nil {
		t.Fatal("expected duration parse error")
	}
}

func TestBillingModel(t *testing.T) {
	d := &Docker{}
	if d.BillingModel() != provider.BillingPerSecond {
		t.Errorf("BillingModel = %v, want per-second", d.BillingModel())
	}
}

// containerID is a canned container ID used across provision/destroy tests.
const (
	containerID = "abc123def456"
	workerImage = "example/worker:latest"
)

// testImage is the canned image string used in tests where the exact value
// does not matter (only that one is set).
const testImage = "img"

func TestProvisionSendsRunCommand(t *testing.T) {
	fc := &fakeCLI{
		responses: []fakeResponse{{stdout: []byte(containerID + "\n")}},
	}
	d := &Docker{cli: fc, cfg: config{Image: workerImage, Network: "my-net"}}
	inst, err := d.Provision(context.Background(), provider.Spec{
		Tag:  "my-tag",
		Name: "fj-bellows-x",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if inst.ID != containerID {
		t.Errorf("ID = %q", inst.ID)
	}
	if inst.Tag != "my-tag" {
		t.Errorf("Tag = %q", inst.Tag)
	}
	if inst.IPv4 != "" {
		t.Errorf("IPv4 should be empty for docker provider, got %q", inst.IPv4)
	}
	if inst.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}

	calls := fc.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	want := []string{
		"run", "-d",
		"--label", "fj-bellows.tag=my-tag",
		"--name", "fj-bellows-x",
		"--network", "my-net",
		workerImage,
	}
	if strings.Join(calls[0].args, " ") != strings.Join(want, " ") {
		t.Errorf("docker args = %v, want %v", calls[0].args, want)
	}
}

func TestProvisionAppendsVolumes(t *testing.T) {
	fc := &fakeCLI{
		responses: []fakeResponse{{stdout: []byte(containerID + "\n")}},
	}
	d := &Docker{cli: fc, cfg: config{
		Image:   workerImage,
		Volumes: []string{"/var/run/docker.sock:/var/run/docker.sock", "/tmp/cache:/cache:ro"},
	}}
	if _, err := d.Provision(context.Background(), provider.Spec{Tag: "t"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	calls := fc.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	want := []string{
		"run", "-d",
		"--label", "fj-bellows.tag=t",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", "/tmp/cache:/cache:ro",
		workerImage,
	}
	if strings.Join(calls[0].args, " ") != strings.Join(want, " ") {
		t.Errorf("docker args = %v, want %v", calls[0].args, want)
	}
}

func TestProvisionEmptyOutputErrors(t *testing.T) {
	fc := &fakeCLI{responses: []fakeResponse{{stdout: []byte("  \n")}}}
	d := &Docker{cli: fc, cfg: config{Image: testImage}}
	if _, err := d.Provision(context.Background(), provider.Spec{Tag: "t"}); err == nil {
		t.Fatal("expected error for empty container id")
	}
}

func TestProvisionPropagatesErr(t *testing.T) {
	wantErr := errors.New("boom")
	fc := &fakeCLI{responses: []fakeResponse{{err: wantErr}}}
	d := &Docker{cli: fc, cfg: config{Image: testImage}}
	_, err := d.Provision(context.Background(), provider.Spec{Tag: "t"})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wraps %v", err, wantErr)
	}
}

func TestDestroyForceRemoves(t *testing.T) {
	fc := &fakeCLI{responses: []fakeResponse{{stdout: []byte(containerID)}}}
	d := &Docker{cli: fc, cfg: config{Image: testImage}}
	if err := d.Destroy(context.Background(), containerID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	calls := fc.snapshot()
	if len(calls) != 1 || strings.Join(calls[0].args, " ") != "rm -f "+containerID {
		t.Errorf("destroy args = %v", calls[0].args)
	}
}

func TestListParsesPSOutput(t *testing.T) {
	out := []byte("" +
		"abc\tname-a\t2026-05-22 12:00:00 +0000 UTC\n" +
		"def\tname-b\t2026-05-22 12:30:00 +0000 UTC\n")
	fc := &fakeCLI{responses: []fakeResponse{{stdout: out}}}
	d := &Docker{cli: fc, cfg: config{Image: testImage}}
	got, err := d.List(context.Background(), "my-tag")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d instances, want 2", len(got))
	}
	if got[0].ID != "abc" || got[0].Name != "name-a" || got[0].Tag != "my-tag" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[0].CreatedAt.IsZero() {
		t.Error("expected parsed CreatedAt")
	}

	calls := fc.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].args[0] != "ps" {
		t.Errorf("expected ps, got %v", calls[0].args)
	}
	// Confirm the label filter is correctly built.
	joined := strings.Join(calls[0].args, " ")
	if !strings.Contains(joined, "label=fj-bellows.tag=my-tag") {
		t.Errorf("label filter missing: %s", joined)
	}
}

func TestListEmptyOutput(t *testing.T) {
	fc := &fakeCLI{responses: []fakeResponse{{stdout: []byte("")}}}
	d := &Docker{cli: fc, cfg: config{Image: testImage}}
	got, err := d.List(context.Background(), "tag")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no instances, got %d", len(got))
	}
}

func TestParseDockerTimeFallback(t *testing.T) {
	// Unknown layout returns zero time, not an error.
	if got := parseDockerTime("nonsense"); !got.IsZero() {
		t.Errorf("parseDockerTime nonsense = %v, want zero", got)
	}
	// Known docker layout parses.
	got := parseDockerTime("2026-05-22 12:00:00 +0000 UTC")
	if got.IsZero() {
		t.Error("expected parsed time")
	}
}
