package config

import (
	"os"
	"strings"
	"testing"
)

// loadExample reads config.example.yaml, swaps in test secrets, and applies
// the supplied line transform before writing it to a temp file and loading.
// Centralises the two parse-the-example tests' shared scaffolding so each
// stays small enough for the gocyclo/funlen budgets.
func loadExample(t *testing.T, transform func(line string, inBlock *bool) (out string, keep bool)) *Config {
	t.Helper()
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	s = strings.ReplaceAll(s, "<forgejo-admin-token>", "tok")
	s = strings.ReplaceAll(s, "<linode-pat>", "pat")
	out := []string{}
	inBlock := false
	for line := range strings.SplitSeq(s, "\n") {
		l, keep := transform(line, &inBlock)
		if keep {
			out = append(out, l)
		}
	}
	path := writeTemp(t, "config.yaml", strings.Join(out, "\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// TestExampleParsesAsSSHDefault — config.example.yaml as shipped (transport
// block commented out) loads cleanly and defaults to ssh mode.
func TestExampleParsesAsSSHDefault(t *testing.T) {
	cfg := loadExample(t, func(line string, _ *bool) (string, bool) {
		return line, true // identity transform
	})
	if cfg.Transport.Mode != TransportSSH {
		t.Errorf("Transport.Mode = %q, want %q", cfg.Transport.Mode, TransportSSH)
	}
}

// uncommentTransportBlock strips the leading "# " from lines in the
// commented `# transport:` block of config.example.yaml. State machine
// runs over the file line-by-line; inBlock tracks whether the transport
// header has been seen and the closing blank line has not.
func uncommentTransportBlock(line string, inBlock *bool) (string, bool) {
	if strings.HasPrefix(line, "# transport:") {
		*inBlock = true
		return strings.TrimPrefix(line, "# "), true
	}
	if !*inBlock {
		return line, true
	}
	if strings.TrimSpace(line) == "" {
		*inBlock = false
		return line, true
	}
	switch {
	case strings.HasPrefix(line, "#   "):
		return line[2:], true // keep two spaces of indent
	case strings.HasPrefix(line, "# "):
		return line[2:], true
	case strings.HasPrefix(line, "#"):
		return line[1:], true
	default:
		*inBlock = false
		return line, true
	}
}

// TestExampleParsesWithTransportUncommented — uncomment the transport block
// in config.example.yaml and verify it parses + validates.
func TestExampleParsesWithTransportUncommented(t *testing.T) {
	cfg := loadExample(t, uncommentTransportBlock)
	if cfg.Transport.Mode != TransportCacheGateway {
		t.Errorf("Transport.Mode = %q, want %q", cfg.Transport.Mode, TransportCacheGateway)
	}
	if cfg.Transport.Tunnel == nil {
		t.Fatal("Transport.Tunnel = nil")
	}
	if len(cfg.Transport.Tunnel.Routes) == 0 {
		t.Error("Tunnel.Routes is empty")
	}
	if len(cfg.Transport.Tunnel.LANEgress) == 0 {
		t.Error("Tunnel.LANEgress is empty")
	}
}
