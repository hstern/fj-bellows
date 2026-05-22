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
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to a single Forgejo instance, scoped to one runner owner.
type Client struct {
	base   string // <url>/api/v1/<scope>
	token  string
	labels []string
	hc     *http.Client
}

// New builds a client. scope is the API path segment that owns the runners,
// e.g. "orgs/example" or "repos/owner/name". labels are the pool labels the
// client filters the job queue by — the Forgejo /actions/runners/jobs endpoint
// requires a non-empty labels query and returns null otherwise.
func New(rawURL, scope, token string, labels ...string) *Client {
	base := strings.TrimRight(rawURL, "/") + "/api/v1/" + strings.Trim(scope, "/")
	return &Client{
		base:   base,
		token:  token,
		labels: labels,
		hc:     &http.Client{Timeout: 30 * time.Second},
	}
}

// WaitingJobs returns jobs currently waiting for a runner that match the
// client's pool labels. The Forgejo endpoint requires a labels filter; with
// no labels configured the endpoint returns null and this method returns nil.
func (c *Client) WaitingJobs(ctx context.Context) ([]WaitingJob, error) {
	path := "/actions/runners/jobs"
	if len(c.labels) > 0 {
		path += "?labels=" + url.QueryEscape(strings.Join(c.labels, ","))
	}
	raw, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	// The live endpoint returns a bare JSON array of ActionRunJob (or null
	// when no jobs match). Tolerate a wrapped {"jobs": [...]} shape too in
	// case a future Forgejo version adds an envelope.
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '{' {
		var env struct {
			Jobs []WaitingJob `json:"jobs"`
		}
		if err := json.Unmarshal(trimmed, &env); err == nil {
			return env.Jobs, nil
		}
	}
	var arr []WaitingJob
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return nil, fmt.Errorf("decode jobs: %w", err)
	}
	return arr, nil
}

// RegisterEphemeral registers a one-shot ephemeral runner and returns its uuid
// and registration token.
//
// The REST endpoint this calls (POST /actions/runners with {"ephemeral":true})
// is a Forgejo 15+ feature. Forgejo <= 12 returns 404; this function surfaces
// that as a clear "needs Forgejo >= 15" error so the operator can read the
// log rather than a generic decode failure.
func (c *Client) RegisterEphemeral(ctx context.Context, name string, labels []string) (Registration, error) {
	body, _ := json.Marshal(map[string]any{
		"ephemeral": true,
		"name":      name,
		"labels":    labels,
	})
	raw, err := c.do(ctx, http.MethodPost, "/actions/runners", body)
	if err != nil {
		if strings.Contains(err.Error(), "status 404") {
			return Registration{}, fmt.Errorf(
				"ephemeral runner registration is unavailable on this Forgejo "+
					"(requires Forgejo >= 15): %w", err)
		}
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

// ListRunners returns the runners registered under the scope.
//
// The GET /actions/runners listing endpoint is a Forgejo 15+ addition;
// Forgejo <= 12 returns 404. The orchestrator's zombie-runner reaper depends
// on this endpoint, but a 404 is not a hard error — it just means there is
// nothing to reap. Return (nil, nil) in that case so older Forgejos do not
// flood the log on every poll.
func (c *Client) ListRunners(ctx context.Context) ([]Runner, error) {
	raw, err := c.do(ctx, http.MethodGet, "/actions/runners", nil)
	if err != nil {
		if strings.Contains(err.Error(), "status 404") {
			return nil, nil
		}
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '{' {
		var env struct {
			Runners []Runner `json:"runners"`
		}
		if err := json.Unmarshal(trimmed, &env); err == nil && env.Runners != nil {
			return env.Runners, nil
		}
	}
	var arr []Runner
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return nil, fmt.Errorf("decode runners: %w", err)
	}
	return arr, nil
}

// DeleteRunner removes a runner registration by id.
func (c *Client) DeleteRunner(ctx context.Context, id int64) error {
	_, err := c.do(ctx, http.MethodDelete, "/actions/runners/"+strconv.FormatInt(id, 10), nil)
	return err
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
