package config

import (
	"strings"
	"testing"
	"time"
)

const validBaseConfig = `
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
    routes: [10.99.0.0/24]
`

func TestWG_FullyConfigured(t *testing.T) {
	path := writeTemp(t, "config.yaml", validBaseConfig+`
  wg:
    private_key_file: /etc/fj-bellows/wg-private-key
    local_addr: 10.99.0.1/32
    keepalive_interval: 1s
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      endpoint: 172.234.203.50:51820
      allowed_ips:
        - 10.99.0.2/32
        - 10.0.0.0/24
    proxies:
      - { listen: 10.99.0.1:443, upstream: git.stern.ca:443 }
      - { listen: 10.99.0.1:22,  upstream: git.stern.ca:22  }
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Transport.WG == nil {
		t.Fatal("Transport.WG is nil")
	}
	wg := cfg.Transport.WG
	if wg.LocalAddr != "10.99.0.1/32" {
		t.Errorf("LocalAddr = %q", wg.LocalAddr)
	}
	if wg.KeepaliveInterval.D() != 1*time.Second {
		t.Errorf("KeepaliveInterval = %v, want 1s", wg.KeepaliveInterval.D())
	}
	if len(wg.Peer.AllowedIPs) != 2 {
		t.Errorf("len(AllowedIPs) = %d, want 2", len(wg.Peer.AllowedIPs))
	}
	if len(wg.Proxies) != 2 {
		t.Errorf("len(Proxies) = %d, want 2", len(wg.Proxies))
	}
}

// Default keepalive is 1s (DefaultWGKeepaliveInterval) when omitted —
// reflects FJB-78's "default to 1000ms" decision.
func TestWG_DefaultKeepaliveOneSecond(t *testing.T) {
	path := writeTemp(t, "config.yaml", validBaseConfig+`
  wg:
    private_key_file: /etc/fj-bellows/wg-private-key
    local_addr: 10.99.0.1/32
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      endpoint: 172.234.203.50:51820
      allowed_ips: [10.99.0.2/32]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Transport.WG.KeepaliveInterval.D(); got != 1*time.Second {
		t.Errorf("default keepalive = %v, want 1s", got)
	}
}

//nolint:funlen // table-driven: 11 small validation cases inline for readability.
func TestWG_Validation(t *testing.T) {
	cases := []struct {
		name    string
		wgBlock string
		wantSub string
	}{
		{
			name: "missing private_key_file",
			wgBlock: `
  wg:
    local_addr: 10.99.0.1/32
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      endpoint: 172.234.203.50:51820
      allowed_ips: [10.99.0.2/32]
`,
			wantSub: "private_key_file is required",
		},
		{
			name: "missing local_addr",
			wgBlock: `
  wg:
    private_key_file: /tmp/k
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      endpoint: 172.234.203.50:51820
      allowed_ips: [10.99.0.2/32]
`,
			wantSub: "local_addr is required",
		},
		{
			name: "bad local_addr",
			wgBlock: `
  wg:
    private_key_file: /tmp/k
    local_addr: not-a-cidr
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      endpoint: 172.234.203.50:51820
      allowed_ips: [10.99.0.2/32]
`,
			wantSub: `local_addr = "not-a-cidr"`,
		},
		{
			name: "missing peer public key",
			wgBlock: `
  wg:
    private_key_file: /tmp/k
    local_addr: 10.99.0.1/32
    peer:
      endpoint: 172.234.203.50:51820
      allowed_ips: [10.99.0.2/32]
`,
			wantSub: "public_key is required",
		},
		{
			name: "missing peer endpoint",
			wgBlock: `
  wg:
    private_key_file: /tmp/k
    local_addr: 10.99.0.1/32
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      allowed_ips: [10.99.0.2/32]
`,
			wantSub: "endpoint is required",
		},
		{
			name: "bad peer endpoint shape",
			wgBlock: `
  wg:
    private_key_file: /tmp/k
    local_addr: 10.99.0.1/32
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      endpoint: not-host-port
      allowed_ips: [10.99.0.2/32]
`,
			wantSub: `endpoint "not-host-port" is not host:port`,
		},
		{
			name: "empty allowed_ips",
			wgBlock: `
  wg:
    private_key_file: /tmp/k
    local_addr: 10.99.0.1/32
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      endpoint: 172.234.203.50:51820
      allowed_ips: []
`,
			wantSub: "allowed_ips must list at least one CIDR",
		},
		{
			name: "bad allowed_ips CIDR",
			wgBlock: `
  wg:
    private_key_file: /tmp/k
    local_addr: 10.99.0.1/32
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      endpoint: 172.234.203.50:51820
      allowed_ips: [bad-cidr]
`,
			wantSub: `allowed_ips[0] = "bad-cidr"`,
		},
		{
			name: "proxy missing listen",
			wgBlock: `
  wg:
    private_key_file: /tmp/k
    local_addr: 10.99.0.1/32
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      endpoint: 172.234.203.50:51820
      allowed_ips: [10.99.0.2/32]
    proxies:
      - { upstream: git.stern.ca:443 }
`,
			wantSub: "listen is required",
		},
		{
			name: "proxy missing upstream",
			wgBlock: `
  wg:
    private_key_file: /tmp/k
    local_addr: 10.99.0.1/32
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      endpoint: 172.234.203.50:51820
      allowed_ips: [10.99.0.2/32]
    proxies:
      - { listen: 10.99.0.1:443 }
`,
			wantSub: "upstream is required",
		},
		{
			name: "negative keepalive",
			wgBlock: `
  wg:
    private_key_file: /tmp/k
    local_addr: 10.99.0.1/32
    keepalive_interval: -1s
    peer:
      public_key: AbcDefGhiJklMnoPqrStuVwxYzAbcDefGhiJklMnoPqs=
      endpoint: 172.234.203.50:51820
      allowed_ips: [10.99.0.2/32]
`,
			wantSub: "keepalive_interval must be non-negative",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, "config.yaml", validBaseConfig+tc.wgBlock)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load: want error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// SSH mode ignores stray wg blocks — matches the existing tunnel-block
// tolerance (operators may toggle modes mid-edit).
func TestWG_IgnoredInSSHMode(t *testing.T) {
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
  wg:
    private_key_file: /tmp/should-not-be-validated
    local_addr: not-a-cidr-but-ignored-in-ssh-mode
`)
	if _, err := Load(path); err != nil {
		t.Errorf("Load: %v (wg block under ssh mode should be ignored)", err)
	}
}
