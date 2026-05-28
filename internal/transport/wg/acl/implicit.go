package acl

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// ImplicitEntries returns the ACL strings the orchestrator injects
// automatically:
//
//   - tcp://<forgejo-host>:<port>  — derived from the Forgejo base URL.
//     The port defaults to 443 for https, 80 for http, 22 for ssh. The
//     scheme is always "tcp" regardless of the URL scheme — the proxy
//     speaks raw TCP and TLS terminates end-to-end with the real upstream.
//   - udp://<orchestrator-wg-addr>:53
//   - tcp://<orchestrator-wg-addr>:53
//
// The last two bind the orchestrator's DNS responder so worker DNS
// queries route over WG back to the orchestrator (see
// docs/designs/transport.md § "Internal-name DNS responder").
//
// Strings the operator already wrote that duplicate any implicit entry
// dedupe automatically when passed through DedupeRaw.
func ImplicitEntries(forgejoBaseURL string, orchestratorWGAddr netip.Addr) ([]string, error) {
	host, port, err := forgejoHostPort(forgejoBaseURL)
	if err != nil {
		return nil, fmt.Errorf("acl: forgejo base url %q: %w", forgejoBaseURL, err)
	}
	if !orchestratorWGAddr.IsValid() {
		return nil, errors.New("acl: invalid orchestrator WG address")
	}
	orch := orchestratorWGAddr.String()
	if orchestratorWGAddr.Is6() && !orchestratorWGAddr.Is4In6() {
		orch = "[" + orch + "]"
	}
	return []string{
		fmt.Sprintf("tcp://%s:%d", host, port),
		fmt.Sprintf("udp://%s:53", orch),
		fmt.Sprintf("tcp://%s:53", orch),
	}, nil
}

// DedupeRaw returns operator + implicit ACL strings concatenated, with
// duplicates removed by canonical Parse round-trip. Order is preserved
// (operator first, then implicit) so operator overrides take ordering
// precedence in any downstream walk.
//
// Returns the parsed Entry slice alongside the deduped string list so
// the caller can use whichever shape it needs.
func DedupeRaw(operator, implicit []string) ([]string, []Entry, error) {
	all := make([]string, 0, len(operator)+len(implicit))
	all = append(all, operator...)
	all = append(all, implicit...)

	entries, err := Parse(all)
	if err != nil {
		return nil, nil, err
	}

	seen := make(map[string]struct{}, len(entries))
	outRaw := make([]string, 0, len(entries))
	outEntries := make([]Entry, 0, len(entries))
	for i, e := range entries {
		key := e.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		outRaw = append(outRaw, all[i])
		outEntries = append(outEntries, e)
	}
	return outRaw, outEntries, nil
}

// forgejoHostPort extracts (host, port) from a Forgejo base URL,
// supplying the per-scheme default port when the URL omits one. The
// returned host has no brackets — callers re-bracket for IPv6 if
// formatting an ACL string.
func forgejoHostPort(rawURL string) (string, int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", 0, err
	}
	if u.Host == "" {
		return "", 0, errors.New("missing host")
	}
	host := u.Hostname()
	if host == "" {
		return "", 0, errors.New("missing host")
	}
	if p := u.Port(); p != "" {
		var port int
		if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
			return "", 0, fmt.Errorf("invalid port %q", p)
		}
		if port < 1 || port > 65535 {
			return "", 0, fmt.Errorf("port %d out of range", port)
		}
		return host, port, nil
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return host, 443, nil
	case "http":
		return host, 80, nil
	case "ssh", "git+ssh":
		return host, 22, nil
	case "":
		return "", 0, errors.New("missing scheme")
	default:
		return "", 0, fmt.Errorf("unsupported scheme %q (want http, https, or ssh)", u.Scheme)
	}
}
