// Package acl is the FJB-54 access-control list — the operator-declared
// allow-list of (protocol, host, port-or-icmp-spec) entries that gates
// what workers may reach through the orchestrator's transparent proxy.
//
// The package has three responsibilities:
//
//  1. Parse the ACL grammar (see docs/designs/transport.md) into typed
//     Entry values.
//  2. Resolve domain entries via the orchestrator host's stdlib resolver
//     and re-resolve at min(rrset TTL) (Resolver / Registry).
//  3. Publish a Snapshot of the current address set + lookup function
//     so downstream consumers (netstack listeners, cache renderer,
//     worker cloud-init) can subscribe to change events.
//
// The package does not enforce ACLs at runtime — it only computes the
// reachability surface. The proxy layer in
// internal/transport/wg/proxy.go performs the per-flow lookup via the
// Snapshot returned here.
package acl

import (
	"net/netip"
	"strconv"
	"strings"
)

// Scheme identifies the L4/L3 protocol an Entry applies to. Only the
// three values declared as constants here are accepted by the parser.
type Scheme string

// Scheme constants. The parser rejects anything else.
const (
	SchemeTCP  Scheme = "tcp"
	SchemeUDP  Scheme = "udp"
	SchemeICMP Scheme = "icmp"
)

// DefaultScheme is applied when an entry omits the scheme prefix (per
// the grammar: `[("tcp" | "udp") "://"] host [":" portspec]` — the
// brackets make the scheme optional, default TCP). ICMP must always be
// explicit because its trailing spec shape differs structurally.
const DefaultScheme = SchemeTCP

// ICMPAny is the sentinel value the parser emits for "any" in an icmp
// type-or-code position (i.e. `icmp://host:any` or `icmp://host:8/any`).
// At enforcement time, -1 means "match any value in that field".
const ICMPAny = -1

// PortRange is one inclusive port range from a portspec — `lo == hi`
// for a single port, `lo < hi` for a range. Always 1-65535.
type PortRange struct {
	Lo int
	Hi int
}

// Contains reports whether port p falls inside the inclusive range.
func (r PortRange) Contains(p int) bool {
	return p >= r.Lo && p <= r.Hi
}

// String formats the range as the parser would accept it ("443" or
// "1024-3000").
func (r PortRange) String() string {
	if r.Lo == r.Hi {
		return strconv.Itoa(r.Lo)
	}
	return strconv.Itoa(r.Lo) + "-" + strconv.Itoa(r.Hi)
}

// TypeCode is one ICMP type[/code] pair. Type and Code are 0-255 (uint8
// in the protocol) or ICMPAny (-1) when the parser saw the "any"
// keyword.
//
// V1 runtime constraint: the proxy refuses to enforce entries whose
// type set isn't a subset of {0 (echo reply), 8 (echo request)}. The
// parser accepts the full grammar so config files don't break when the
// runtime constraint relaxes; HasUnsupportedICMPTypes on the Entry
// surfaces the v1-incompatible case so callers can error early.
type TypeCode struct {
	Type int
	Code int
}

// String formats the type[/code] pair as the parser would accept it.
// "any" round-trips to "any".
func (tc TypeCode) String() string {
	t := icmpFieldString(tc.Type)
	c := icmpFieldString(tc.Code)
	if tc.Code == ICMPAny && tc.Type != ICMPAny {
		// Bare type means "any code" implicitly; omit the slash to
		// keep the canonical form matching common usage.
		return t
	}
	return t + "/" + c
}

func icmpFieldString(v int) string {
	if v == ICMPAny {
		return "any"
	}
	return strconv.Itoa(v)
}

// Entry is one parsed ACL row.
//
// Exactly one of PortSpec / ICMPSpec is non-nil — selected by Scheme.
// Host is the literal grammar token (post-bracket-strip for IPv6); for
// IP literals and CIDRs it is also reflected in Prefix. For domain
// entries, Prefix is the zero value and the DNS resolver loop fills in
// the address set at run time.
type Entry struct {
	// Scheme is "tcp", "udp", or "icmp".
	Scheme Scheme

	// Host is the original host token (e.g. "git.stern.ca", "192.0.2.1",
	// "10.0.0.0/24", "dead:beef::1", "dead:beef::/64"). Brackets around
	// IPv6 addresses are stripped — they're a grammar concession to
	// disambiguate ":" in the colon-separated portspec, not part of
	// the host's identity.
	Host string

	// Prefix is the parsed IP literal or CIDR. The zero value
	// (IsDomain() == true) means Host is a DNS name to be resolved.
	// Single-IP literals are normalized to a /32 (IPv4) or /128 (IPv6)
	// prefix so downstream code has one shape to operate on.
	Prefix netip.Prefix

	// PortSpec is non-nil for tcp/udp entries. nil for icmp entries.
	// Empty slice means "all ports" — though the parser currently
	// requires at least one port-or-range when ":portspec" is present.
	// An entry without ":portspec" gets a single PortRange{1, 65535}
	// to keep the lookup path branch-free.
	PortSpec []PortRange

	// ICMPSpec is non-nil for icmp entries. nil for tcp/udp. Empty
	// slice means "all types and codes" (i.e. the operator wrote
	// `icmp://host` with no `:icmpspec`).
	ICMPSpec []TypeCode

	// Original is the exact string the operator wrote, preserved for
	// diagnostics + dedup.
	Original string
}

// IsDomain reports whether this entry's host is a DNS name (i.e. needs
// resolution before its prefix(es) are known).
func (e Entry) IsDomain() bool {
	return !e.Prefix.IsValid()
}

// HasUnsupportedICMPTypes reports whether the entry's ICMP type set
// contains anything outside {0, 8} after expanding "any" type to the
// full 0-255 range. V1 runtime supports echo only — this lets callers
// surface a clear error at boot rather than at packet time.
//
// Returns false for tcp/udp entries.
func (e Entry) HasUnsupportedICMPTypes() bool {
	if e.Scheme != SchemeICMP {
		return false
	}
	for _, tc := range e.ICMPSpec {
		if tc.Type == ICMPAny {
			// "any type" includes things outside {0, 8} by definition.
			return true
		}
		if tc.Type != 0 && tc.Type != 8 {
			return true
		}
	}
	return false
}

// String renders the entry in canonical form (one of the grammar's
// accepted spellings). Round-trips through Parse.
func (e Entry) String() string {
	var b strings.Builder
	b.WriteString(string(e.Scheme))
	b.WriteString("://")
	if e.isIPv6Host() {
		b.WriteByte('[')
		b.WriteString(e.Host)
		b.WriteByte(']')
	} else {
		b.WriteString(e.Host)
	}
	if e.Scheme == SchemeICMP {
		if len(e.ICMPSpec) > 0 {
			b.WriteByte(':')
			for i, tc := range e.ICMPSpec {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(tc.String())
			}
		}
		return b.String()
	}
	if len(e.PortSpec) > 0 && !isAllPorts(e.PortSpec) {
		b.WriteByte(':')
		for i, r := range e.PortSpec {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(r.String())
		}
	}
	return b.String()
}

func (e Entry) isIPv6Host() bool {
	if !e.Prefix.IsValid() {
		return false
	}
	return e.Prefix.Addr().Is6() && !e.Prefix.Addr().Is4In6()
}

func isAllPorts(ps []PortRange) bool {
	return len(ps) == 1 && ps[0].Lo == 1 && ps[0].Hi == 65535
}
