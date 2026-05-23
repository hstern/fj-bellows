package linode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// httpDoer is the slice of *http.Client tests need to fake — keeps the resolver
// trivially mockable without standing up a httptest server.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// externalIPProbe holds the endpoints used by resolveExternalIP. icanhazip is
// Cloudflare-operated, returns just the IP in the response body, and exposes
// distinct v4/v6 hostnames so each address family is probed independently.
type externalIPProbe struct {
	v4URL  string
	v6URL  string
	client httpDoer
}

func defaultExternalIPProbe() externalIPProbe {
	return externalIPProbe{
		v4URL:  "https://ipv4.icanhazip.com",
		v6URL:  "https://ipv6.icanhazip.com",
		client: http.DefaultClient,
	}
}

// resolveExternalIP returns CIDR strings for the orchestrator's external IPv4
// (`/32`) and IPv6 (`/128`) addresses, fetched via icanhazip. Either may be
// absent if that address family isn't reachable from the host (a v4-only or
// v6-only network is normal — return whatever resolved). Returns an error
// only if BOTH probes fail: the caller treats that as a fatal Configure-time
// failure so an operator doesn't silently end up with a managed firewall that
// allows nobody in.
func resolveExternalIP(ctx context.Context, p externalIPProbe) (cidrs []string, err error) {
	v4, v4err := probeAddress(ctx, p.client, p.v4URL, 4)
	v6, v6err := probeAddress(ctx, p.client, p.v6URL, 6)
	if v4 != "" {
		cidrs = append(cidrs, v4+"/32")
	}
	if v6 != "" {
		cidrs = append(cidrs, v6+"/128")
	}
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("auto: both v4 and v6 probes failed: %w", errors.Join(v4err, v6err))
	}
	return cidrs, nil
}

// probeAddress GETs url, parses the response body as an IP literal, and
// requires it to match the expected family. Empty string + error on failure.
func probeAddress(ctx context.Context, c httpDoer, url string, family int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	// icanhazip returns the IP plus a trailing newline. Cap the read so a
	// rogue endpoint can't stream gigabytes at us.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(string(body))
	ip := net.ParseIP(raw)
	if ip == nil {
		return "", fmt.Errorf("not an IP literal: %q", raw)
	}
	switch family {
	case 4:
		if ip.To4() == nil {
			return "", errors.New("expected IPv4, got non-v4")
		}
		return ip.String(), nil
	case 6:
		// To4() returns non-nil for v4-mapped-v6; for a true v6 address it
		// is nil. We want only true v6 in the /128 bucket.
		if ip.To4() != nil {
			return "", errors.New("expected IPv6, got v4")
		}
		return ip.String(), nil
	}
	return "", fmt.Errorf("unsupported family %d", family)
}
