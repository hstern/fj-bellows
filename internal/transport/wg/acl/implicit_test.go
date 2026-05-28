package acl

import (
	"net/netip"
	"strings"
	"testing"
)

func TestImplicitEntries_HTTPSDefault(t *testing.T) {
	got, err := ImplicitEntries("https://git.stern.ca", netip.MustParseAddr("100.64.0.1"))
	if err != nil {
		t.Fatalf("ImplicitEntries: %v", err)
	}
	want := []string{
		"tcp://git.stern.ca:443",
		udpDNS,
		"tcp://100.64.0.1:53",
	}
	assertEqualStrings(t, got, want)
}

func TestImplicitEntries_PortOverride(t *testing.T) {
	got, err := ImplicitEntries("https://git.stern.ca:8443", netip.MustParseAddr("100.64.0.1"))
	if err != nil {
		t.Fatalf("ImplicitEntries: %v", err)
	}
	if got[0] != "tcp://git.stern.ca:8443" {
		t.Errorf("forgejo entry = %q, want tcp://git.stern.ca:8443", got[0])
	}
}

func TestImplicitEntries_SSHDefault(t *testing.T) {
	got, err := ImplicitEntries("ssh://git.stern.ca", netip.MustParseAddr("100.64.0.1"))
	if err != nil {
		t.Fatalf("ImplicitEntries: %v", err)
	}
	if got[0] != "tcp://git.stern.ca:22" {
		t.Errorf("forgejo entry = %q, want tcp://git.stern.ca:22", got[0])
	}
}

func TestImplicitEntries_HTTPDefault(t *testing.T) {
	got, err := ImplicitEntries("http://git.stern.ca", netip.MustParseAddr("100.64.0.1"))
	if err != nil {
		t.Fatalf("ImplicitEntries: %v", err)
	}
	if got[0] != "tcp://git.stern.ca:80" {
		t.Errorf("forgejo entry = %q, want tcp://git.stern.ca:80", got[0])
	}
}

func TestImplicitEntries_BadURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"empty", "", missingHost},
		{"no scheme", "git.stern.ca", missingHost},
		{"unsupported scheme", "ftp://git.stern.ca", "unsupported scheme"},
		{"bad port", "https://git.stern.ca:notaport", "invalid port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ImplicitEntries(tc.url, netip.MustParseAddr("100.64.0.1"))
			if err == nil {
				t.Fatalf("want error %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestImplicitEntries_IPv6OrchestratorAddr(t *testing.T) {
	got, err := ImplicitEntries("https://git.stern.ca", netip.MustParseAddr("fdfb::1"))
	if err != nil {
		t.Fatalf("ImplicitEntries: %v", err)
	}
	if got[1] != "udp://[fdfb::1]:53" || got[2] != "tcp://[fdfb::1]:53" {
		t.Errorf("ipv6 dns entries = %v, want bracketed", got[1:])
	}
}

func TestDedupeRaw_DropsOperatorEcho(t *testing.T) {
	op := []string{
		"tcp://git.stern.ca:443", // duplicate of implicit
		tcpNexus,
	}
	imp, err := ImplicitEntries("https://git.stern.ca", netip.MustParseAddr("100.64.0.1"))
	if err != nil {
		t.Fatalf("ImplicitEntries: %v", err)
	}
	raw, entries, err := DedupeRaw(op, imp)
	if err != nil {
		t.Fatalf("DedupeRaw: %v", err)
	}
	if len(raw) != len(entries) {
		t.Fatalf("len(raw)=%d != len(entries)=%d", len(raw), len(entries))
	}
	// Operator's tcp://git.stern.ca:443 stays in (came first); implicit
	// duplicate drops. nexus, both dns implicits remain.
	want := []string{
		"tcp://git.stern.ca:443",
		tcpNexus,
		udpDNS,
		"tcp://100.64.0.1:53",
	}
	assertEqualStrings(t, raw, want)
}

func assertEqualStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
