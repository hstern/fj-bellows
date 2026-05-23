package orchestrator

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strconv"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"simple":              "'simple'",
		"with space":          "'with space'",
		"https://x.example/y": "'https://x.example/y'",
		"a'b":                 `'a'\''b'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// Verify SSHDispatcher satisfies the Dispatcher interface.
var _ Dispatcher = (*SSHDispatcher)(nil)

// newTestHostKey generates a fresh ed25519 SSH public key.
func newTestHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}
	return pk
}

func TestTOFUHostKeyCallback(t *testing.T) {
	keyA := newTestHostKey(t)
	keyB := newTestHostKey(t)
	if string(keyA.Marshal()) == string(keyB.Marshal()) {
		t.Fatal("generated keys are not distinct")
	}

	d := &SSHDispatcher{}
	const addr1 = "10.0.0.1:22"
	const addr2 = "10.0.0.2:22"
	cb1 := d.tofuHostKeyCallback(addr1)

	// First use records and accepts.
	if err := cb1("", nil, keyA); err != nil {
		t.Fatalf("first contact should be accepted: %v", err)
	}
	// Same key on a subsequent dial accepts.
	if err := cb1("", nil, keyA); err != nil {
		t.Fatalf("matching pinned key should be accepted: %v", err)
	}
	// A different key for the same addr is rejected (possible MITM).
	if err := cb1("", nil, keyB); err == nil {
		t.Fatal("mismatched host key for pinned addr should be rejected")
	}

	// Different addrs are independent: addr2 may pin keyB on its first contact.
	cb2 := d.tofuHostKeyCallback(addr2)
	if err := cb2("", nil, keyB); err != nil {
		t.Fatalf("first contact on distinct addr should be accepted: %v", err)
	}
	if err := cb2("", nil, keyA); err == nil {
		t.Fatal("mismatched host key for second addr should be rejected")
	}
	// addr1 pin is unaffected by addr2 activity.
	if err := cb1("", nil, keyA); err != nil {
		t.Fatalf("addr1 pin should remain valid: %v", err)
	}
}

// Verify SSHDispatcher satisfies the HostKeyPinner interface.
var _ HostKeyPinner = (*SSHDispatcher)(nil)

// hostInternal is the generic placeholder used across parse/hosts-override
// tests. Kept as a const so goconst doesn't flag the repeated literal.
const hostInternal = "forgejo.internal"

func TestParseForgejoURL(t *testing.T) {
	cases := []struct {
		in        string
		wantHost  string
		wantPort  int
		wantIPLit bool
		wantErr   bool
	}{
		{in: "https://" + hostInternal, wantHost: hostInternal, wantPort: 443},
		{in: "http://" + hostInternal, wantHost: hostInternal, wantPort: 80},
		{in: "http://localhost:3000", wantHost: "localhost", wantPort: 3000},
		{in: "https://" + hostInternal + ":8443/", wantHost: hostInternal, wantPort: 8443},
		{in: "http://192.0.2.10:8080", wantHost: "192.0.2.10", wantPort: 8080, wantIPLit: true},
		{in: "https://[2001:db8::1]:8443", wantHost: "2001:db8::1", wantPort: 8443, wantIPLit: true},
		{in: "ftp://" + hostInternal, wantErr: true}, // unsupported scheme
		{in: "https://", wantErr: true},              // no host
		{in: "http://" + hostInternal + ":0", wantErr: true},
		{in: "http://" + hostInternal + ":99999", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseForgejoURL(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.host != c.wantHost {
				t.Errorf("host = %q, want %q", got.host, c.wantHost)
			}
			if got.port != c.wantPort {
				t.Errorf("port = %d, want %d", got.port, c.wantPort)
			}
			if got.isIPLit != c.wantIPLit {
				t.Errorf("isIPLit = %v, want %v", got.isIPLit, c.wantIPLit)
			}
		})
	}
}

func TestHostsOverrideCommand(t *testing.T) {
	cases := []struct {
		name string
		in   forgejoTarget
		want string
	}{
		{
			name: "hostname needs override",
			in:   forgejoTarget{host: "forgejo.internal", port: 443},
			want: "grep -qF '127.0.0.1 forgejo.internal' /etc/hosts || echo '127.0.0.1 forgejo.internal' >> /etc/hosts",
		},
		{
			name: "localhost already mapped",
			in:   forgejoTarget{host: "localhost", port: 3000},
			want: "",
		},
		{
			name: "Localhost case-insensitive",
			in:   forgejoTarget{host: "LOCALHOST", port: 3000},
			want: "",
		},
		{
			name: "IPv4 literal needs no DNS",
			in:   forgejoTarget{host: "192.0.2.10", port: 8080, isIPLit: true},
			want: "",
		},
		{
			name: "IPv6 literal needs no DNS",
			in:   forgejoTarget{host: "2001:db8::1", port: 8443, isIPLit: true},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hostsOverrideCommand(c.in); got != c.want {
				t.Errorf("hostsOverrideCommand(%+v)\n  got:  %q\n  want: %q", c.in, got, c.want)
			}
		})
	}
}

func TestPinHostKeyRequiresSeededKeyOnFirstContact(t *testing.T) {
	keyA := newTestHostKey(t)
	keyB := newTestHostKey(t)

	const ip = "10.0.0.7"
	const port = 2222

	// Seeded pin: the very first contact must REQUIRE the seeded key, distinct
	// from unseeded TOFU which would accept (and record) whatever is presented.
	d := &SSHDispatcher{Port: port}
	d.PinHostKey(ip, keyA)
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	cb := d.tofuHostKeyCallback(addr)

	// First contact with a mismatching key is rejected (no TOFU recording).
	if err := cb("", nil, keyB); err == nil {
		t.Fatal("seeded pin must reject a mismatching key on first contact")
	}
	// First contact with the seeded key is accepted.
	if err := cb("", nil, keyA); err != nil {
		t.Fatalf("seeded pin must accept the matching key on first contact: %v", err)
	}

	// Contrast: an unseeded dispatcher accepts the first key it sees (TOFU).
	d2 := &SSHDispatcher{Port: port}
	addr2 := net.JoinHostPort(ip, strconv.Itoa(port))
	cb2 := d2.tofuHostKeyCallback(addr2)
	if err := cb2("", nil, keyB); err != nil {
		t.Fatalf("unseeded TOFU should accept first contact: %v", err)
	}
}
