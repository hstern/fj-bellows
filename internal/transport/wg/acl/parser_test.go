package acl

import (
	"net/netip"
	"strings"
	"testing"
)

// Shared test constants — kept in one place so goconst doesn't trip
// on table-row literals that intentionally repeat across cases.
const (
	tcpCIDR     = "tcp://10.0.0.0/24:443"
	tcpDomain   = "tcp://api.example.com:443"
	tcpNexus    = "tcp://nexus.stern.ca:80,443"
	udpDNS      = "udp://100.64.0.1:53"
	outOfRange  = "out of range"
	exampleHost = "api.example.com"
	missingHost = "missing host"
	v6Net       = "dead:beef::/64"
	v4Net24     = "192.168.0.0/24"
)

type acceptCase struct {
	name    string
	input   string
	scheme  Scheme
	host    string
	prefix  string // expected Prefix.String(); "" = domain entry
	ports   []PortRange
	icmps   []TypeCode
	isV6Lit bool
}

// acceptedCases enumerates every grammar form spelled out in
// docs/designs/transport.md § ACL Grammar + § Examples. Lives at file
// scope so the test function stays under funlen's threshold.
//
//nolint:gochecknoglobals // table data; the alternative is a 100-line test body.
var acceptedCases = []acceptCase{
	{
		name:   "tcp domain single port",
		input:  "tcp://forgejo.stern.ca:22",
		scheme: SchemeTCP,
		host:   "forgejo.stern.ca",
		ports:  []PortRange{{Lo: 22, Hi: 22}},
	},
	{
		name:   "default scheme is tcp",
		input:  "git.stern.ca:443",
		scheme: SchemeTCP,
		host:   "git.stern.ca",
		ports:  []PortRange{{Lo: 443, Hi: 443}},
	},
	{
		name:   "tcp domain multi port",
		input:  tcpNexus,
		scheme: SchemeTCP,
		host:   "nexus.stern.ca",
		ports:  []PortRange{{Lo: 80, Hi: 80}, {Lo: 443, Hi: 443}},
	},
	{
		name:   "tcp port range",
		input:  "tcp://nexus.stern.ca:8000-8099",
		scheme: SchemeTCP,
		host:   "nexus.stern.ca",
		ports:  []PortRange{{Lo: 8000, Hi: 8099}},
	},
	{
		name:   "udp ipv4 literal",
		input:  udpDNS,
		scheme: SchemeUDP,
		host:   "100.64.0.1",
		prefix: "100.64.0.1/32",
		ports:  []PortRange{{Lo: 53, Hi: 53}},
	},
	{
		name:   "tcp ipv4 cidr wildcard",
		input:  "tcp://172.16.0.0/16:443",
		scheme: SchemeTCP,
		host:   "172.16.0.0/16",
		prefix: "172.16.0.0/16",
		ports:  []PortRange{{Lo: 443, Hi: 443}},
	},
	{
		name:    "tcp ipv6 literal",
		input:   "tcp://[dead:beef::1]:443",
		scheme:  SchemeTCP,
		host:    "dead:beef::1",
		prefix:  "dead:beef::1/128",
		ports:   []PortRange{{Lo: 443, Hi: 443}},
		isV6Lit: true,
	},
	{
		name:    "tcp ipv6 cidr multi range",
		input:   "tcp://[" + v6Net + "]:1024-3000,8000-8099",
		scheme:  SchemeTCP,
		host:    v6Net,
		prefix:  v6Net,
		ports:   []PortRange{{Lo: 1024, Hi: 3000}, {Lo: 8000, Hi: 8099}},
		isV6Lit: true,
	},
	{
		name:    "icmp ipv6 cidr multi type-code",
		input:   "icmp://[" + v6Net + "]:8/0,0/0",
		scheme:  SchemeICMP,
		host:    v6Net,
		prefix:  v6Net,
		icmps:   []TypeCode{{Type: 8, Code: 0}, {Type: 0, Code: 0}},
		isV6Lit: true,
	},
	{
		name:   "icmp ipv4 cidr type only",
		input:  "icmp://" + v4Net24 + ":8",
		scheme: SchemeICMP,
		host:   v4Net24,
		prefix: v4Net24,
		icmps:  []TypeCode{{Type: 8, Code: ICMPAny}},
	},
	{
		name:   "icmp any type any code",
		input:  "icmp://" + v4Net24 + ":any/any",
		scheme: SchemeICMP,
		host:   v4Net24,
		prefix: v4Net24,
		icmps:  []TypeCode{{Type: ICMPAny, Code: ICMPAny}},
	},
	{
		name:   "icmp bare domain",
		input:  "icmp://ping.stern.ca",
		scheme: SchemeICMP,
		host:   "ping.stern.ca",
		icmps:  nil, // no spec → match all
	},
	{
		name:   "tcp bare ipv4 = all ports",
		input:  "tcp://10.0.0.1",
		scheme: SchemeTCP,
		host:   "10.0.0.1",
		prefix: "10.0.0.1/32",
		ports:  []PortRange{{Lo: 1, Hi: 65535}},
	},
}

func TestParse_AcceptedForms(t *testing.T) {
	for _, tc := range acceptedCases {
		t.Run(tc.name, func(t *testing.T) {
			checkAcceptCase(t, tc)
		})
	}
}

func checkAcceptCase(t *testing.T, tc acceptCase) {
	t.Helper()
	out, err := Parse([]string{tc.input})
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", tc.input, err)
	}
	if len(out) != 1 {
		t.Fatalf("Parse(%q) len = %d, want 1", tc.input, len(out))
	}
	e := out[0]
	if e.Scheme != tc.scheme {
		t.Errorf("scheme = %q, want %q", e.Scheme, tc.scheme)
	}
	if e.Host != tc.host {
		t.Errorf("host = %q, want %q", e.Host, tc.host)
	}
	checkPrefix(t, e, tc.prefix)
	if !portsEqual(e.PortSpec, tc.ports) {
		t.Errorf("PortSpec = %v, want %v", e.PortSpec, tc.ports)
	}
	if !icmpsEqual(e.ICMPSpec, tc.icmps) {
		t.Errorf("ICMPSpec = %v, want %v", e.ICMPSpec, tc.icmps)
	}
	if e.Original != tc.input {
		t.Errorf("Original = %q, want %q", e.Original, tc.input)
	}
}

func checkPrefix(t *testing.T, e Entry, want string) {
	t.Helper()
	if want == "" {
		if !e.IsDomain() {
			t.Errorf("want domain entry, got prefix %v", e.Prefix)
		}
		return
	}
	if e.IsDomain() {
		t.Errorf("want literal entry, got domain")
		return
	}
	if got := e.Prefix.String(); got != want {
		t.Errorf("prefix = %q, want %q", got, want)
	}
}

// TestParse_RejectedForms exercises every rejection rule named in the
// grammar/parser doc. One row per rule keeps the failure messages
// pointed at the exact precondition that fired.
func TestParse_RejectedForms(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"unknown scheme", "ftp://example.com:21", "unknown scheme"},
		{"https scheme", "https://example.com:443", "unknown scheme"},
		{"empty", "", "empty entry"},
		{missingHost, "tcp://", missingHost},
		{"unmatched bracket", "tcp://[dead:beef::1", "unmatched '["},
		{"port too high", "tcp://example.com:70000", outOfRange},
		{"port too low", "tcp://example.com:0", outOfRange},
		{"port nonint", "tcp://example.com:abc", "not an integer"},
		{"range descending", "tcp://example.com:1000-500", "lo 1000 > hi 500"},
		{"range overlapping", "tcp://example.com:1000-2000,1500-3000", "overlapping ranges"},
		{"empty portspec", "tcp://example.com:", "empty spec"},
		{"empty range part", "tcp://example.com:1000,,2000", "empty port-or-range"},
		// portspec on icmp: parser tries to read it as icmpspec —
		// "443" parses as type 443 which exceeds 255.
		{"icmp with portspec-like int", "icmp://192.168.0.0/24:443", outOfRange},
		{"icmp empty spec", "icmp://192.168.0.0/24:", "empty spec"},
		{"icmp type out of range", "icmp://192.168.0.0/24:300", outOfRange},
		{"icmp code out of range", "icmp://192.168.0.0/24:8/300", outOfRange},
		{"bad cidr", "tcp://10.0.0.0/40:443", "invalid CIDR"},
		{"bad domain", "tcp://no$dots:443", "neither an IP, CIDR, nor a valid domain"},
		{"bad ipv6 literal", "tcp://[not::an::ipv6]:443", "neither an IP, CIDR"},
		{"trailing garbage after bracket", "tcp://[dead:beef::1]xx:443", "unexpected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]string{tc.input})
			if err == nil {
				t.Fatalf("Parse(%q): want error containing %q, got nil", tc.input, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse(%q) error = %v, want substring %q", tc.input, err, tc.want)
			}
		})
	}
}

// TestEntry_HasUnsupportedICMPTypes covers the v1 runtime constraint
// (echo only — types 0 and 8). The parser accepts the full grammar so
// configs don't break later; this method lets boot code surface the
// v1-incompatible case with a useful error.
func TestEntry_HasUnsupportedICMPTypes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"echo only", "icmp://1.2.3.4:8/0,0/0", false},
		{"echo request only", "icmp://1.2.3.4:8", false},
		{"echo reply only", "icmp://1.2.3.4:0", false},
		{"unreachable type", "icmp://1.2.3.4:3", true},
		{"any type", "icmp://1.2.3.4:any", true},
		{"mixed echo + unreachable", "icmp://1.2.3.4:8,3", true},
		{"tcp ignored", "tcp://1.2.3.4:443", false},
		{"icmp no spec means all types", "icmp://1.2.3.4", false}, // empty spec = no specific types declared → falls through to "all"
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Parse([]string{tc.in})
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if got := out[0].HasUnsupportedICMPTypes(); got != tc.want {
				t.Errorf("HasUnsupportedICMPTypes(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestEntry_StringRoundTrip ensures every entry the parser accepts
// formats back into a string the parser also accepts. This is the
// foundation of DedupeRaw's canonical-form dedup.
func TestEntry_StringRoundTrip(t *testing.T) {
	inputs := []string{
		"tcp://forgejo.stern.ca:22",
		"tcp://nexus.stern.ca:80,443",
		"tcp://192.168.1.2:5432",
		"tcp://172.16.0.0/16:443",
		"udp://100.64.0.1:53",
		"icmp://192.168.0.0/24:8/0,0/0",
		"tcp://[dead:beef::1]:443",
		"tcp://[dead:beef::/64]:1024-3000,8000-8099",
		"icmp://[dead:beef::/64]:8/0,0/0",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			first, err := Parse([]string{in})
			if err != nil {
				t.Fatalf("first Parse: %v", err)
			}
			canon := first[0].String()
			second, err := Parse([]string{canon})
			if err != nil {
				t.Fatalf("re-Parse(%q): %v", canon, err)
			}
			if first[0].String() != second[0].String() {
				t.Errorf("not idempotent: %q -> %q -> %q", in, first[0].String(), second[0].String())
			}
		})
	}
}

func portsEqual(a, b []PortRange) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func icmpsEqual(a, b []TypeCode) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParse_PrefixNormalisation makes sure a literal IP becomes a /32
// (or /128) Prefix so downstream code never has to special-case
// "literal-as-Prefix" vs "CIDR-as-Prefix".
func TestParse_PrefixNormalisation(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"tcp://1.2.3.4:443", "1.2.3.4/32"},
		{"tcp://[dead:beef::1]:443", "dead:beef::1/128"},
		{tcpCIDR, "10.0.0.0/24"},
		{"tcp://[dead:beef::/64]:443", "dead:beef::/64"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			out, err := Parse([]string{tc.in})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := out[0].Prefix.String(); got != tc.want {
				t.Errorf("Prefix = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPortRange_Contains exercises the inclusive-range matcher.
func TestPortRange_Contains(t *testing.T) {
	r := PortRange{Lo: 80, Hi: 100}
	cases := []struct {
		port int
		want bool
	}{
		{79, false},
		{80, true},
		{90, true},
		{100, true},
		{101, false},
	}
	for _, tc := range cases {
		if got := r.Contains(tc.port); got != tc.want {
			t.Errorf("Contains(%d) = %v, want %v", tc.port, got, tc.want)
		}
	}
}

// Sanity: parsed IP literal Prefix is sane regardless of which netip
// representation the user passes. (4-in-6 corner case isn't supported
// by the grammar, but the v6 path should still produce a useful
// String().)
func TestParse_IPv6PrefixString(t *testing.T) {
	out, err := Parse([]string{"tcp://[2001:db8::1]:443"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := netip.MustParseAddr("2001:db8::1")
	if out[0].Prefix.Addr() != want {
		t.Errorf("Prefix.Addr() = %v, want %v", out[0].Prefix.Addr(), want)
	}
}
