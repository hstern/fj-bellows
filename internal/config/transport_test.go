package config

import (
	"strings"
	"testing"
)

// TestTransportDefaults — empty Transport block defaults to "ssh" so existing
// configs keep working unchanged.
func TestTransportDefaults(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo:
  url: https://forgejo.example.com
  token: tok
  scope: orgs/example
  labels: [ubuntu-latest]
provider: linode
provider_config:
  region: us-ord
  type: g6-nanode-1
ssh:
  private_key_file: /tmp/id
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Transport.Mode != TransportSSH {
		t.Errorf("Transport.Mode = %q, want %q", cfg.Transport.Mode, TransportSSH)
	}
	if cfg.Transport.Tunnel != nil {
		t.Errorf("Transport.Tunnel = %#v, want nil for ssh mode", cfg.Transport.Tunnel)
	}
}

// TestTransportCacheGatewayValid — full cache-gateway block parses + validates.
func TestTransportCacheGatewayValid(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo:
  url: https://forgejo.example.com
  token: tok
  scope: orgs/example
  labels: [ubuntu-latest]
provider: linode
provider_config:
  region: us-ord
  type: g6-nanode-1
ssh:
  private_key_file: /tmp/id
transport:
  mode: cache-gateway
  tunnel:
    routes:
      - 192.168.0.0/24
      - 10.10.0.0/16
    lan_egress:
      - { from: worker-vpc, to: 192.168.0.2, port: 53,  proto: udp }
      - { from: worker-vpc, to: 192.168.0.2, port: 53,  proto: tcp }
      - { from: worker-vpc, to: 192.168.0.7, port: 443, proto: tcp }
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Transport.Mode != TransportCacheGateway {
		t.Errorf("Transport.Mode = %q, want %q", cfg.Transport.Mode, TransportCacheGateway)
	}
	if cfg.Transport.Tunnel == nil {
		t.Fatalf("Transport.Tunnel is nil")
	}
	if got, want := len(cfg.Transport.Tunnel.Routes), 2; got != want {
		t.Errorf("len(routes) = %d, want %d", got, want)
	}
	if got, want := len(cfg.Transport.Tunnel.LANEgress), 3; got != want {
		t.Errorf("len(lan_egress) = %d, want %d", got, want)
	}
	first := cfg.Transport.Tunnel.LANEgress[0]
	if first.From != EgressFromWorkerVPC || first.To != "192.168.0.2" || first.Port != 53 || first.Proto != "udp" {
		t.Errorf("lan_egress[0] = %#v, parsed wrong", first)
	}
}

// TestTransportCacheGatewayValidation — each invalid input gets a focused error.
func TestTransportCacheGatewayValidation(t *testing.T) {
	tests := []struct {
		name      string
		transport string
		wantErr   string
	}{
		{
			name: "unknown mode",
			transport: `transport:
  mode: tailscale`,
			wantErr: `unknown mode "tailscale"`,
		},
		{
			name: "cache-gateway without tunnel block",
			transport: `transport:
  mode: cache-gateway`,
			wantErr: `requires a transport.tunnel block`,
		},
		{
			name: "empty routes",
			transport: `transport:
  mode: cache-gateway
  tunnel:
    routes: []`,
			wantErr: `routes must list at least one CIDR`,
		},
		{
			name: "bad CIDR",
			transport: `transport:
  mode: cache-gateway
  tunnel:
    routes: ["not-a-cidr"]`,
			wantErr: `routes[0] = "not-a-cidr"`,
		},
		{
			name: "missing from",
			transport: `transport:
  mode: cache-gateway
  tunnel:
    routes: [192.168.0.0/24]
    lan_egress:
      - { to: 192.168.0.2, port: 53, proto: udp }`,
			wantErr: `from is required`,
		},
		{
			name: "unknown from",
			transport: `transport:
  mode: cache-gateway
  tunnel:
    routes: [192.168.0.0/24]
    lan_egress:
      - { from: cache-vpc, to: 192.168.0.2, port: 53, proto: udp }`,
			wantErr: `unknown from "cache-vpc"`,
		},
		{
			name: "missing to",
			transport: `transport:
  mode: cache-gateway
  tunnel:
    routes: [192.168.0.0/24]
    lan_egress:
      - { from: worker-vpc, port: 53, proto: udp }`,
			wantErr: `to is required`,
		},
		{
			name: "bad to",
			transport: `transport:
  mode: cache-gateway
  tunnel:
    routes: [192.168.0.0/24]
    lan_egress:
      - { from: worker-vpc, to: not-an-ip, port: 53, proto: udp }`,
			wantErr: `to "not-an-ip" is not a valid IP`,
		},
		{
			name: "port too low",
			transport: `transport:
  mode: cache-gateway
  tunnel:
    routes: [192.168.0.0/24]
    lan_egress:
      - { from: worker-vpc, to: 192.168.0.2, port: 0, proto: udp }`,
			wantErr: `port 0 out of range`,
		},
		{
			name: "port too high",
			transport: `transport:
  mode: cache-gateway
  tunnel:
    routes: [192.168.0.0/24]
    lan_egress:
      - { from: worker-vpc, to: 192.168.0.2, port: 99999, proto: udp }`,
			wantErr: `port 99999 out of range`,
		},
		{
			name: "missing proto",
			transport: `transport:
  mode: cache-gateway
  tunnel:
    routes: [192.168.0.0/24]
    lan_egress:
      - { from: worker-vpc, to: 192.168.0.2, port: 53 }`,
			wantErr: `proto is required`,
		},
		{
			name: "bad proto",
			transport: `transport:
  mode: cache-gateway
  tunnel:
    routes: [192.168.0.0/24]
    lan_egress:
      - { from: worker-vpc, to: 192.168.0.2, port: 53, proto: sctp }`,
			wantErr: `proto "sctp" must be "tcp" or "udp"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, "config.yaml", `
forgejo:
  url: https://forgejo.example.com
  token: tok
  scope: orgs/example
  labels: [ubuntu-latest]
provider: linode
provider_config:
  region: us-ord
  type: g6-nanode-1
ssh:
  private_key_file: /tmp/id
`+tt.transport+"\n")
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load: want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestTransportProtoCaseInsensitive — proto "TCP" / "UDP" should also work.
func TestTransportProtoCaseInsensitive(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo:
  url: https://forgejo.example.com
  token: tok
  scope: orgs/example
  labels: [ubuntu-latest]
provider: linode
provider_config:
  region: us-ord
  type: g6-nanode-1
ssh:
  private_key_file: /tmp/id
transport:
  mode: cache-gateway
  tunnel:
    routes: [192.168.0.0/24]
    lan_egress:
      - { from: worker-vpc, to: 192.168.0.2, port: 53, proto: TCP }
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v (uppercase proto should be accepted)", err)
	}
}

// TestTransportSSHIgnoresTunnel — having a stray tunnel block under mode "ssh"
// is allowed (operators may toggle modes mid-edit); it just doesn't apply.
func TestTransportSSHIgnoresTunnel(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
forgejo:
  url: https://forgejo.example.com
  token: tok
  scope: orgs/example
  labels: [ubuntu-latest]
provider: linode
provider_config:
  region: us-ord
  type: g6-nanode-1
ssh:
  private_key_file: /tmp/id
transport:
  mode: ssh
  tunnel:
    routes: [bad-cidr-that-would-fail-validation]
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load with ssh mode + stray tunnel: %v (expected no validation of tunnel under ssh)", err)
	}
}
