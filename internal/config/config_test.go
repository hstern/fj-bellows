package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndDeferredDecode(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo:
  url: https://forgejo.example.com
  token: secret-token
  scope: orgs/example
  labels: [ubuntu-latest]
provider: linode
provider_config:
  region: example-region
  type: example-type
ssh:
  private_key_file: /tmp/id
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tag != "fj-bellows" {
		t.Errorf("default tag = %q", cfg.Tag)
	}
	if cfg.Scale.Max != 1 {
		t.Errorf("default max = %d", cfg.Scale.Max)
	}
	if cfg.Poll.Interval.D() != 10*time.Second {
		t.Errorf("default interval = %s", cfg.Poll.Interval.D())
	}
	if cfg.SSH.User != "root" || cfg.SSH.Port != 22 {
		t.Errorf("ssh defaults = %q:%d", cfg.SSH.User, cfg.SSH.Port)
	}

	// provider_config must survive as a decodable node, opaque to core.
	var pc struct {
		Region string `yaml:"region"`
		Type   string `yaml:"type"`
	}
	if err := cfg.ProviderConfig.Decode(&pc); err != nil {
		t.Fatalf("decode provider_config: %v", err)
	}
	if pc.Region != "example-region" || pc.Type != "example-type" {
		t.Errorf("provider_config = %+v", pc)
	}
}

func TestLoadDurations(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo: {url: u, token: t, scope: orgs/x}
provider: linode
ssh: {private_key_file: k}
poll:
  interval: 30s
  idle_timeout: 2m
  hour_margin: 90s
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Poll.Interval.D() != 30*time.Second {
		t.Errorf("interval = %s", cfg.Poll.Interval.D())
	}
	if cfg.Poll.IdleTimeout.D() != 2*time.Minute {
		t.Errorf("idle = %s", cfg.Poll.IdleTimeout.D())
	}
	if cfg.Poll.HourMargin.D() != 90*time.Second {
		t.Errorf("margin = %s", cfg.Poll.HourMargin.D())
	}
}

func TestLoadMissingRequired(t *testing.T) {
	path := writeTemp(t, "config.yaml", `provider: linode`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestLoadBadDuration(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo: {url: u, token: t, scope: s}
provider: linode
ssh: {private_key_file: k}
poll: {interval: not-a-duration}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected duration parse error")
	}
}
