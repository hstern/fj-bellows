package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRedact_NilReturnsNil(t *testing.T) {
	if Redact(nil) != nil {
		t.Fatal("Redact(nil) should be nil")
	}
}

func TestRedact_ForgejoTokenReplaced(t *testing.T) {
	cfg := &Config{Forgejo: Forgejo{Token: "real-admin-token"}}
	got := Redact(cfg)
	if got.Forgejo.Token != redactedMarker {
		t.Errorf("Forgejo.Token = %q; want %q", got.Forgejo.Token, redactedMarker)
	}
	if cfg.Forgejo.Token != "real-admin-token" {
		t.Errorf("original mutated: Forgejo.Token = %q", cfg.Forgejo.Token)
	}
}

func TestRedact_EmptyForgejoTokenStaysEmpty(t *testing.T) {
	// An unset token shouldn't be stamped with the marker — that would
	// lie to the operator about whether they configured one.
	cfg := &Config{}
	got := Redact(cfg)
	if got.Forgejo.Token != "" {
		t.Errorf("empty token got marker: %q", got.Forgejo.Token)
	}
}

func TestRedact_SSHKeyPathPassesThrough(t *testing.T) {
	// The path itself isn't a secret — only the file at that path.
	cfg := &Config{SSH: SSH{PrivateKeyFile: "/etc/fj-bellows/id_ed25519"}}
	got := Redact(cfg)
	if got.SSH.PrivateKeyFile != "/etc/fj-bellows/id_ed25519" {
		t.Errorf("PrivateKeyFile got redacted: %q", got.SSH.PrivateKeyFile)
	}
}

func TestRedact_ProviderConfig_LinodeShape(t *testing.T) {
	// Mirror the Linode shape from config.example.yaml: a top-level token,
	// nested mappings (firewall, vpc, cache) and sequences. Only the
	// `token` value should change; everything else passes through.
	src := `
region: us-ord
type: g6-nanode-1
image: linode/debian13
token: real-linode-pat
firewall:
  allow_inbound:
    - auto
placement_group:
  enforcement: flexible
cache:
  tls:
    ca_dir: /var/lib/fj-bellows/cache-ca
`
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(src), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg := &Config{ProviderConfig: node}
	got := Redact(cfg)

	out, err := yaml.Marshal(&got.ProviderConfig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "token: <redacted>") {
		t.Errorf("token not redacted; output:\n%s", s)
	}
	if strings.Contains(s, "real-linode-pat") {
		t.Errorf("token value leaked:\n%s", s)
	}
	if !strings.Contains(s, "region: us-ord") {
		t.Errorf("region was clobbered; output:\n%s", s)
	}
	if !strings.Contains(s, "ca_dir: /var/lib/fj-bellows/cache-ca") {
		t.Errorf("non-secret path clobbered; output:\n%s", s)
	}

	// Original must be untouched (Redact must deep-copy the node tree).
	origOut, _ := yaml.Marshal(&cfg.ProviderConfig)
	if !strings.Contains(string(origOut), "real-linode-pat") {
		t.Errorf("original was mutated:\n%s", origOut)
	}
}

func TestRedact_ProviderConfig_DockerShape(t *testing.T) {
	// Docker config has no nested secrets — everything must pass through.
	src := `
image: example/worker:latest
runtime: docker
`
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(src), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg := &Config{ProviderConfig: node}
	got := Redact(cfg)
	out, _ := yaml.Marshal(&got.ProviderConfig)
	s := string(out)
	if !strings.Contains(s, "image: example/worker:latest") {
		t.Errorf("image was lost:\n%s", s)
	}
	if strings.Contains(s, "<redacted>") {
		t.Errorf("docker shape produced a redaction marker:\n%s", s)
	}
}

func TestRedact_AllSecretKeyVariants(t *testing.T) {
	// Pin every name in secretKeyNames so adding one to the list above
	// without a test won't regress unnoticed.
	src := `
token: t1
password: p1
secret: s1
key: k1
api_key: a1
access_key: ak1
secret_key: sk1
region: us-east
`
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(src), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg := &Config{ProviderConfig: node}
	got := Redact(cfg)
	out, _ := yaml.Marshal(&got.ProviderConfig)
	s := string(out)
	for _, leaked := range []string{"t1", "p1", "s1", "k1", "a1", "ak1", "sk1"} {
		if strings.Contains(s, ": "+leaked+"\n") || strings.HasSuffix(strings.TrimSpace(s), ": "+leaked) {
			t.Errorf("value %q leaked through:\n%s", leaked, s)
		}
	}
	if !strings.Contains(s, "region: us-east") {
		t.Errorf("non-secret key was clobbered:\n%s", s)
	}
}

func TestRedact_SecretKeyMatchingIsCaseInsensitiveButExact(t *testing.T) {
	// "TOKEN" matches; "tokenizer" does not (no substring matching).
	src := `
TOKEN: real-upper
tokenizer: harmless-substring-match
SecretRecipe: harmless-substring-match
`
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(src), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg := &Config{ProviderConfig: node}
	got := Redact(cfg)
	out, _ := yaml.Marshal(&got.ProviderConfig)
	s := string(out)
	if strings.Contains(s, "real-upper") {
		t.Errorf("TOKEN should match case-insensitively:\n%s", s)
	}
	if !strings.Contains(s, "tokenizer: harmless-substring-match") {
		t.Errorf("tokenizer should NOT be redacted:\n%s", s)
	}
	if !strings.Contains(s, "SecretRecipe: harmless-substring-match") {
		t.Errorf("SecretRecipe should NOT be redacted:\n%s", s)
	}
}
