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

const testHostKeyPEM = `-----BEGIN OPENSSH PRIVATE KEY-----
dGVzdC1ub3QtYS1yZWFsLWtleQ==
-----END OPENSSH PRIVATE KEY-----`

func TestRenderWithHostKey(t *testing.T) {
	out, err := Render(Params{RunnerVersion: "1.0.0", HostPrivateKey: testHostKeyPEM})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The host key install must target the ed25519 host key path with 0600 mode.
	if !strings.Contains(out, "/etc/ssh/ssh_host_ed25519_key") {
		t.Error("host key path missing")
	}
	if !strings.Contains(out, "chmod 0600 /etc/ssh/ssh_host_ed25519_key") {
		t.Error("host key 0600 mode missing")
	}
	// The derived public key and sshd HostKey config must be written.
	if !strings.Contains(out, "ssh-keygen -y -f /etc/ssh/ssh_host_ed25519_key") {
		t.Error("public key derivation missing")
	}
	if !strings.Contains(out, "HostKey /etc/ssh/ssh_host_ed25519_key") {
		t.Error("sshd HostKey directive missing")
	}
	// sshd must be reloaded/restarted so it picks up the injected host key.
	if !strings.Contains(out, "reload sshd") && !strings.Contains(out, "restart sshd") {
		t.Error("sshd reload/restart missing")
	}
	// The PEM is injected base64-encoded; the raw PEM header must not appear.
	if strings.Contains(out, "BEGIN OPENSSH PRIVATE KEY") {
		t.Error("raw PEM leaked into user-data; expected base64 encoding")
	}
	// Arch detection must still be intact alongside the host-key block.
	if !strings.Contains(out, "x86_64") || !strings.Contains(out, "aarch64") {
		t.Error("arch detection missing when host key provided")
	}
}

func TestRenderWithoutHostKeyUnchanged(t *testing.T) {
	out, err := Render(Params{RunnerVersion: "1.0.0"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "ssh_host_ed25519_key") {
		t.Error("host key install present when no host key was provided")
	}
	if !strings.Contains(out, "x86_64") || !strings.Contains(out, "aarch64") {
		t.Error("arch detection missing")
	}
}
