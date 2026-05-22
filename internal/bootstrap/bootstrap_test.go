package bootstrap

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	out, err := Render(Params{RunnerVersion: "12.10.1"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "#cloud-config") {
		t.Error("missing cloud-config header")
	}
	if !strings.Contains(out, "v12.10.1/forgejo-runner-12.10.1-linux-") {
		t.Errorf("runner version not templated:\n%s", out)
	}
	if !strings.Contains(out, DefaultReadyFile) {
		t.Error("default ready file not used")
	}
	// arch detection must be present for amd64 and arm64 workers.
	if !strings.Contains(out, "x86_64") || !strings.Contains(out, "aarch64") {
		t.Error("arch detection missing")
	}
}

func TestRenderCustomReadyFile(t *testing.T) {
	out, err := Render(Params{RunnerVersion: "1.0.0", ReadyFile: "/tmp/custom-ready"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "/tmp/custom-ready") {
		t.Error("custom ready file not used")
	}
}

func TestRenderRequiresVersion(t *testing.T) {
	if _, err := Render(Params{}); err == nil {
		t.Fatal("expected error for missing RunnerVersion")
	}
}
