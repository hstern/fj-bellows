package linode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
)

const defaultGithubMetaURL = "https://api.github.com/meta"

// fetchGithubActionsCIDRs returns the CIDRs under the `actions` key of
// https://api.github.com/meta — the egress ranges a GitHub-hosted runner can
// emit traffic from. Used by the `github-actions` allow_inbound sentinel so an
// operator running fj-bellows from a GH-hosted runner can let that runner SSH
// to managed workers via the firewall.
//
// The endpoint requires no auth. Responses are typically dozens of v4 CIDRs +
// a few v6 CIDRs — well under the Linode per-rule cap (255 v4 + 255 v6).
// Each returned string is parse-validated; malformed entries are dropped with
// no warning. An empty result (e.g. the key disappeared) returns an error so
// the caller can treat it as a degenerate fetch.
func fetchGithubActionsCIDRs(ctx context.Context, client httpDoer, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// api.github.com returns nicer rate-limit headers + JSON when we identify
	// ourselves; the User-Agent is the only required header for an
	// unauthenticated GET.
	req.Header.Set("User-Agent", "fj-bellows")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	// The meta endpoint payload is bounded (~64KB today); cap at 2 MiB so a
	// rogue response can't OOM us.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	// Decode just the slice we care about — `actions` is one of many top-level
	// keys (api, hooks, web, packages, ...).
	var payload struct {
		Actions []string `json:"actions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode meta: %w", err)
	}
	out := make([]string, 0, len(payload.Actions))
	for _, s := range payload.Actions {
		if _, _, err := net.ParseCIDR(s); err != nil {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("github-actions: meta.actions returned no usable CIDRs")
	}
	return out, nil
}
