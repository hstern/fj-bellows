package bootstrap

import (
	"strings"
	"testing"
)

const testRunnerVersion = "1.0.0"

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
	out, err := Render(Params{RunnerVersion: testRunnerVersion, ReadyFile: "/tmp/custom-ready"})
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
	out, err := Render(Params{RunnerVersion: testRunnerVersion, HostPrivateKey: testHostKeyPEM})
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
	out, err := Render(Params{RunnerVersion: testRunnerVersion})
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

func TestRenderWithoutFJBAgent_NoAgentArtifacts(t *testing.T) {
	out, err := Render(Params{RunnerVersion: testRunnerVersion})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// When FJBAgentDownloadURL is empty, none of the agent-install
	// artifacts should be present — keeps the base cloud-init backward
	// compatible for deployments that haven't enabled the agent yet.
	for _, needle := range []string{"fjbagent.service", "/etc/fjbagent/auth.token", "useradd", "fjb:fjb"} {
		if strings.Contains(out, needle) {
			t.Errorf("agent artifact %q leaked into agent-disabled render:\n%s", needle, out)
		}
	}
}

func TestRenderWithFJBAgent_ProducesAllArtifacts(t *testing.T) {
	url := "https://example.com/fjbagent-linux-$rarch"
	out, err := Render(Params{
		RunnerVersion:       testRunnerVersion,
		FJBAgentDownloadURL: url,
		FJBAgentToken:       "deadbeefcafe",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	wants := []string{
		// User + group creation via cloud-init's users module.
		"name: fjb",
		"primary_group: fjb",
		"system: true",
		// Token file with correct mode/owner.
		"/etc/fjbagent/auth.token",
		"'0600'",
		"fjb:fjb",
		"deadbeefcafe",
		// Default env file for the systemd unit.
		"/etc/default/fjbagent",
		"FJBAGENT_LISTEN=0.0.0.0:9001",
		// systemd unit text embedded from fjbagent.service.
		"/etc/systemd/system/fjbagent.service",
		"Type=notify",
		"User=fjb",
		"NoNewPrivileges=true",
		// runcmd fetches + enables the service.
		"curl -fsSL -o /usr/local/bin/fjbagent",
		url,
		"systemctl enable --now fjbagent",
	}
	for _, needle := range wants {
		if !strings.Contains(out, needle) {
			t.Errorf("expected %q in cloud-init output:\n%s", needle, out)
		}
	}
}

func TestRenderFJBAgent_RequiresToken(t *testing.T) {
	_, err := Render(Params{
		RunnerVersion:       testRunnerVersion,
		FJBAgentDownloadURL: "https://example.com/fjbagent-linux-$rarch",
		// FJBAgentToken intentionally unset
	})
	if err == nil {
		t.Fatal("expected error when FJBAgentDownloadURL set without token")
	}
	if !strings.Contains(err.Error(), "FJBAgentToken") {
		t.Errorf("error did not mention token: %v", err)
	}
}

func TestResolveAgentDownloadURL(t *testing.T) {
	tests := []struct {
		name, urlTmpl, version, want string
	}{
		{
			name:    "default-template",
			urlTmpl: "",
			version: "0.6.0",
			want:    "https://github.com/hstern/fj-bellows/releases/download/v0.6.0/fjbagent-linux-$rarch",
		},
		{
			name:    "custom-template",
			urlTmpl: "https://internal.example/fjbagent-{{.Version}}-{{.Arch}}",
			version: "0.6.0",
			want:    "https://internal.example/fjbagent-0.6.0-$rarch",
		},
		{
			name:    "no-placeholders",
			urlTmpl: "https://fixed-url.example/fjbagent",
			version: "0.6.0",
			want:    "https://fixed-url.example/fjbagent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveAgentDownloadURL(tt.urlTmpl, tt.version)
			if err != nil {
				t.Fatalf("ResolveAgentDownloadURL: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
