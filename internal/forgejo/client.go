// Package forgejo is a thin REST client for the Forgejo Actions runner API.
// It uses the admin token to poll the job queue and to mint ephemeral runner
// registrations.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to a single Forgejo instance, scoped to one runner owner.
type Client struct {
	base  string // <url>/api/v1/<scope>
	token string
	hc    *http.Client
}

// New builds a client. scope is the API path segment that owns the runners,
// e.g. "orgs/example" or "repos/owner/name".
func New(url, scope, token string) *Client {
	base := strings.TrimRight(url, "/") + "/api/v1/" + strings.Trim(scope, "/")
	return &Client{
		base:  base,
		token: token,
		hc:    &http.Client{Timeout: 30 * time.Second},
	}
}

// WaitingJobs returns jobs currently waiting for a runner.
func (c *Client) WaitingJobs(ctx context.Context) ([]WaitingJob, error) {
	// The endpoint may return either a bare array or an object wrapping it;
	// decode into a small envelope that tolerates both.
	var env struct {
		Jobs []WaitingJob `json:"jobs"`
	}
	raw, err := c.do(ctx, http.MethodGet, "/actions/runners/jobs", nil)
	if err != nil {
		return nil, err
	}
	// Try the wrapped form first, then a bare array.
	if err := json.Unmarshal(raw, &env); err == nil && env.Jobs != nil {
		return env.Jobs, nil
	}
	var arr []WaitingJob
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("decode jobs: %w", err)
	}
	return arr, nil
}

// RegisterEphemeral registers a one-shot ephemeral runner and returns its uuid
// and registration token. Forgejo invalidates these after a single job.
func (c *Client) RegisterEphemeral(ctx context.Context, name string, labels []string) (Registration, error) {
	body, _ := json.Marshal(map[string]any{
		"ephemeral": true,
		"name":      name,
		"labels":    labels,
	})
	raw, err := c.do(ctx, http.MethodPost, "/actions/runners", body)
	if err != nil {
		return Registration{}, err
	}
	var reg Registration
	if err := json.Unmarshal(raw, &reg); err != nil {
		return Registration{}, fmt.Errorf("decode registration: %w", err)
	}
	if reg.UUID == "" || reg.Token == "" {
		return Registration{}, fmt.Errorf("registration response missing uuid/token: %s", raw)
	}
	return reg, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, raw)
	}
	return raw, nil
}
