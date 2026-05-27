package config

import (
	"os"
	"strings"
	"testing"
)

// TestExampleParsesAsSSHDefault — config.example.yaml as shipped (transport
// block commented out) loads cleanly and defaults to ssh mode.
func TestExampleParsesAsSSHDefault(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	s = strings.ReplaceAll(s, "<forgejo-admin-token>", "tok")
	s = strings.ReplaceAll(s, "<linode-pat>", "pat")
	path := writeTemp(t, "config.yaml", s)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Transport.Mode != TransportSSH {
		t.Errorf("Transport.Mode = %q, want %q", cfg.Transport.Mode, TransportSSH)
	}
}

// TestExampleParsesWithTransportUncommented — uncomment the transport block
// in config.example.yaml and verify it parses + validates.
func TestExampleParsesWithTransportUncommented(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	s = strings.ReplaceAll(s, "<forgejo-admin-token>", "tok")
	s = strings.ReplaceAll(s, "<linode-pat>", "pat")
	// Strip the leading "# " on transport block lines. The block uses
	// "# " for documentation and "#   " for nested indent — replace
	// "# transport:" anchor and then strip leading "# " on contiguous
	// lines until the next blank line.
	out := []string{}
	inBlock := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "# transport:") {
			inBlock = true
			out = append(out, strings.TrimPrefix(line, "# "))
			continue
		}
		if inBlock {
			if strings.TrimSpace(line) == "" {
				inBlock = false
				out = append(out, line)
				continue
			}
			// Lines in the commented block start with "# " or "#" only.
			// Strip exactly one "# " (or "#") prefix.
			switch {
			case strings.HasPrefix(line, "#   "):
				out = append(out, line[2:]) // keep two spaces of indent
			case strings.HasPrefix(line, "# "):
				out = append(out, line[2:])
			case strings.HasPrefix(line, "#"):
				out = append(out, line[1:])
			default:
				// Not a comment — block ended without blank line; stop stripping.
				inBlock = false
				out = append(out, line)
			}
			continue
		}
		out = append(out, line)
	}
	path := writeTemp(t, "config.yaml", strings.Join(out, "\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with transport uncommented: %v", err)
	}
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
