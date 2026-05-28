package acl

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Parse turns operator-supplied ACL strings into typed Entry values.
// It returns the first error encountered (with the offending string
// quoted) and leaves error recovery to the caller.
//
// Grammar (locked in docs/designs/transport.md):
//
//	entry            := portspec-entry | icmp-entry
//	portspec-entry   := [("tcp" | "udp") "://"] host [":" portspec]
//	icmp-entry       := "icmp://" host [":" icmpspec]
//	host             := domain | ipv4 | ipv4-cidr | "[" ipv6 "]" | "[" ipv6-cidr "]"
//	portspec         := port-or-range ("," port-or-range)*
//	port-or-range    := port | port "-" port           # inclusive
//	icmpspec         := type-code ("," type-code)*
//	type-code        := icmp-type ["/" icmp-code]      # code omitted = any code
//	icmp-type        := uint8 | "any"
//	icmp-code        := uint8 | "any"
//
// Rejection rules enforced here:
//
//   - Scheme outside {tcp, udp, icmp}.
//   - portspec on an icmp entry; icmpspec on a tcp/udp entry.
//   - Empty portspec / icmpspec after the colon.
//   - Port out of [1, 65535] or non-integer.
//   - Descending ranges (lo > hi), single-port "ranges" (lo == hi
//     using "-" syntax is fine; the parser canonicalises),
//     overlapping ranges across entries in the same portspec.
//   - ICMP type/code outside [0, 255].
//
// V1 runtime icmp-type constraint ({0, 8} only) is NOT enforced here —
// the parser accepts the full grammar so config files don't break when
// the constraint relaxes. Use Entry.HasUnsupportedICMPTypes to detect
// the v1-incompatible case at boot.
func Parse(strs []string) ([]Entry, error) {
	out := make([]Entry, 0, len(strs))
	for _, s := range strs {
		e, err := parseEntry(s)
		if err != nil {
			return nil, fmt.Errorf("acl: %q: %w", s, err)
		}
		out = append(out, e)
	}
	return out, nil
}

func parseEntry(raw string) (Entry, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Entry{}, errors.New("empty entry")
	}

	scheme, rest := splitScheme(s)
	switch scheme {
	case SchemeTCP, SchemeUDP, SchemeICMP:
		// ok
	default:
		return Entry{}, fmt.Errorf("unknown scheme %q (want tcp, udp, or icmp)", scheme)
	}

	host, spec, hasSpec, err := splitHostSpec(rest)
	if err != nil {
		return Entry{}, err
	}

	e := Entry{Scheme: scheme, Host: host, Original: raw}
	if err := fillHost(&e, host); err != nil {
		return Entry{}, err
	}
	if err := fillSpec(&e, spec, hasSpec); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func fillHost(e *Entry, host string) error {
	prefix, isLiteral, err := parseHost(host)
	if err != nil {
		return err
	}
	if !isLiteral {
		return nil
	}
	e.Prefix = prefix
	// Canonicalise IPv6 Host string so dedup works regardless of the
	// operator's chosen spelling.
	if prefix.Addr().Is6() && !prefix.Addr().Is4In6() {
		if prefix.Bits() == prefix.Addr().BitLen() {
			e.Host = prefix.Addr().String()
		} else {
			e.Host = prefix.String()
		}
	}
	return nil
}

func fillSpec(e *Entry, spec string, hasSpec bool) error {
	if e.Scheme == SchemeICMP {
		if !hasSpec {
			// Bare `icmp://host` = match all types/codes. Empty
			// ICMPSpec ≡ "all" at runtime.
			return nil
		}
		tcs, err := parseICMPSpec(spec)
		if err != nil {
			return err
		}
		e.ICMPSpec = tcs
		return nil
	}
	if !hasSpec {
		// Bare `tcp://host` = match all ports. Single 1-65535
		// range keeps lookup branch-free.
		e.PortSpec = []PortRange{{Lo: 1, Hi: 65535}}
		return nil
	}
	ports, err := parsePortSpec(spec)
	if err != nil {
		return err
	}
	e.PortSpec = ports
	return nil
}

// splitScheme returns the scheme + the host[:spec] remainder. Missing
// scheme defaults to DefaultScheme (tcp).
func splitScheme(s string) (Scheme, string) {
	if scheme, rest, ok := strings.Cut(s, "://"); ok {
		return Scheme(strings.ToLower(scheme)), rest
	}
	return DefaultScheme, s
}

// splitHostSpec splits "host" or "host:spec" honouring the `[ ... ]`
// brackets around IPv6 host expressions. Returns (host, spec,
// hasSpec, err). hasSpec distinguishes `host` from `host:` (the
// latter is an error — empty spec).
func splitHostSpec(s string) (host, spec string, hasSpec bool, err error) {
	if s == "" {
		return "", "", false, errors.New("missing host")
	}
	if s[0] == '[' {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return "", "", false, errors.New("unmatched '[' in host expression")
		}
		host = s[1:end]
		rest := s[end+1:]
		switch {
		case rest == "":
			return host, "", false, nil
		case rest[0] == ':':
			spec = rest[1:]
			if spec == "" {
				return "", "", false, errors.New("empty spec after ':'")
			}
			return host, spec, true, nil
		default:
			return "", "", false, fmt.Errorf("unexpected %q after bracketed host", rest)
		}
	}
	// No brackets — split on the last ':' so an IPv4 host like
	// "1.2.3.4:443,80-90" works. (IPv4 has no colons; ports always
	// follow the last colon.)
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		host = s[:i]
		spec = s[i+1:]
		if host == "" {
			return "", "", false, errors.New("missing host")
		}
		if spec == "" {
			return "", "", false, errors.New("empty spec after ':'")
		}
		return host, spec, true, nil
	}
	return s, "", false, nil
}

// parseHost classifies the host token. Returns (prefix, isLiteral, err)
// where isLiteral means the token parsed as an IP / CIDR; !isLiteral
// means the token looks like a DNS name (validated lightly to reject
// obviously broken strings).
func parseHost(host string) (netip.Prefix, bool, error) {
	// CIDR first (because "10.0.0.0/24" would also parse as a domain
	// label otherwise).
	if strings.ContainsRune(host, '/') {
		pfx, err := netip.ParsePrefix(host)
		if err != nil {
			return netip.Prefix{}, false, fmt.Errorf("invalid CIDR %q: %w", host, err)
		}
		return pfx.Masked(), true, nil
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		bits := addr.BitLen()
		return netip.PrefixFrom(addr, bits), true, nil
	}
	// Treat as a domain. Light validation: at least one dot, no
	// disallowed runes. We do NOT do IDNA / full RFC 1035 — the
	// resolver does the heavy lifting and surfaces a real error at
	// resolve time.
	if !looksLikeDomain(host) {
		return netip.Prefix{}, false, fmt.Errorf("host %q is neither an IP, CIDR, nor a valid domain", host)
	}
	return netip.Prefix{}, false, nil
}

func looksLikeDomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return strings.ContainsRune(s, '.')
}

func parsePortSpec(s string) ([]PortRange, error) {
	parts := strings.Split(s, ",")
	out := make([]PortRange, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, errors.New("empty port-or-range in portspec")
		}
		r, err := parsePortOrRange(p)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := validateNoOverlap(out); err != nil {
		return nil, err
	}
	return out, nil
}

func parsePortOrRange(p string) (PortRange, error) {
	if loS, hiS, ok := strings.Cut(p, "-"); ok {
		lo, err := parsePort(loS)
		if err != nil {
			return PortRange{}, fmt.Errorf("range %q: %w", p, err)
		}
		hi, err := parsePort(hiS)
		if err != nil {
			return PortRange{}, fmt.Errorf("range %q: %w", p, err)
		}
		if lo > hi {
			return PortRange{}, fmt.Errorf("range %q: lo %d > hi %d", p, lo, hi)
		}
		return PortRange{Lo: lo, Hi: hi}, nil
	}
	v, err := parsePort(p)
	if err != nil {
		return PortRange{}, err
	}
	return PortRange{Lo: v, Hi: v}, nil
}

func parsePort(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty port")
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("port %q: not an integer", s)
	}
	if v < 1 || v > 65535 {
		return 0, fmt.Errorf("port %d out of range (want 1-65535)", v)
	}
	return v, nil
}

// validateNoOverlap rejects portspecs whose ranges overlap. Operators
// usually intend disjoint sets; an overlap means a typo or copy-paste
// bug, so it's better to fail at config-load than to silently dedupe.
func validateNoOverlap(rs []PortRange) error {
	for i := range rs {
		for j := i + 1; j < len(rs); j++ {
			if rs[i].Lo <= rs[j].Hi && rs[j].Lo <= rs[i].Hi {
				return fmt.Errorf("overlapping ranges in portspec: %s and %s", rs[i], rs[j])
			}
		}
	}
	return nil
}

func parseICMPSpec(s string) ([]TypeCode, error) {
	parts := strings.Split(s, ",")
	out := make([]TypeCode, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, errors.New("empty type-code in icmpspec")
		}
		tc, err := parseTypeCode(p)
		if err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, nil
}

func parseTypeCode(s string) (TypeCode, error) {
	typS, codeS, hasCode := strings.Cut(s, "/")
	t, err := parseICMPField(typS)
	if err != nil {
		return TypeCode{}, fmt.Errorf("icmp type %q: %w", typS, err)
	}
	c := ICMPAny
	if hasCode {
		c, err = parseICMPField(codeS)
		if err != nil {
			return TypeCode{}, fmt.Errorf("icmp code %q: %w", codeS, err)
		}
	}
	return TypeCode{Type: t, Code: c}, nil
}

func parseICMPField(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty field")
	}
	if strings.EqualFold(s, "any") {
		return ICMPAny, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.New("not an integer")
	}
	if v < 0 || v > 255 {
		return 0, fmt.Errorf("value %d out of range (want 0-255)", v)
	}
	return v, nil
}
