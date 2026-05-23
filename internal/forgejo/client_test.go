package forgejo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWaitingJobsBareArray(t *testing.T) {
	// The Forgejo 11.x/12.x /actions/runners/jobs endpoint returns a bare JSON
	// array of ActionRunJob and requires a non-empty labels query.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/actions/runners/jobs") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("labels"); got != "docker,linux" {
			t.Errorf("labels query = %q, want docker,linux", got)
		}
		if got := r.Header.Get("Authorization"); got != "token secret" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = io.WriteString(w, `[{"id":2,"name":"hello","runs_on":["docker","linux"],"status":"waiting","task_id":0}]`)
	}))
	defer srv.Close()

	c := New(srv.URL, "orgs/example", "secret", "docker", "linux")
	jobs, err := c.WaitingJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != 2 || jobs[0].Name != "hello" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if len(jobs[0].Labels) != 2 || jobs[0].Labels[0] != "docker" {
		t.Fatalf("labels = %+v", jobs[0].Labels)
	}
}

func TestWaitingJobsWrappedTolerated(t *testing.T) {
	// Future-proof: if a Forgejo version wraps the response in {"jobs":[...]},
	// the client still decodes it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"jobs":[{"id":1,"handle":"h1","runs_on":["docker"]}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "repos/o/r", "t", "docker")
	jobs, err := c.WaitingJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Handle != "h1" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestWaitingJobsNullResponse(t *testing.T) {
	// Forgejo returns the literal `null` when no jobs match the labels filter.
	// The client treats that as an empty queue, not an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `null`)
	}))
	defer srv.Close()

	c := New(srv.URL, "orgs/x", "t", "docker")
	jobs, err := c.WaitingJobs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want empty", jobs)
	}
}

func TestWaitingJobsNoLabels(t *testing.T) {
	// With no labels configured the client omits the query parameter (and the
	// server typically returns null in that case).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("labels"); got != "" {
			t.Errorf("labels query should be empty, got %q", got)
		}
		_, _ = io.WriteString(w, `null`)
	}))
	defer srv.Close()

	c := New(srv.URL, "orgs/x", "t")
	if _, err := c.WaitingJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterEphemeral(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["ephemeral"] != true {
			t.Errorf("ephemeral not set: %+v", body)
		}
		_, _ = io.WriteString(w, `{"uuid":"u-1","token":"tok-1"}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "orgs/example", "t", "docker")
	reg, err := c.RegisterEphemeral(context.Background(), "runner-x", []string{labelUbuntu})
	if err != nil {
		t.Fatal(err)
	}
	if reg.UUID != "u-1" || reg.Token != "tok-1" {
		t.Fatalf("reg = %+v", reg)
	}
}

func TestRegisterEphemeralMissingFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "orgs/x", "t")
	if _, err := c.RegisterEphemeral(context.Background(), "r", nil); err == nil {
		t.Fatal("expected error for missing uuid/token")
	}
}

func TestRegisterEphemeralForgejo12(t *testing.T) {
	// Forgejo 12 has no POST /actions/runners endpoint and returns 404. The
	// client surfaces that with a "requires Forgejo >= 15" hint so the
	// operator sees a clear diagnostic.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	c := New(srv.URL, "orgs/x", "t")
	_, err := c.RegisterEphemeral(context.Background(), "r", nil)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "Forgejo >= 15") {
		t.Fatalf("error should mention Forgejo >= 15: %v", err)
	}
}

func TestListRunners(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/actions/runners") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"runners":[{"id":7,"uuid":"u-7","name":"fj-bellows-abcd","status":"offline"}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "orgs/example", "t")
	runners, err := c.ListRunners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 1 || runners[0].ID != 7 || runners[0].Name != "fj-bellows-abcd" {
		t.Fatalf("runners = %+v", runners)
	}
}

func TestListRunnersForgejo12(t *testing.T) {
	// Forgejo <= 12 lacks GET /actions/runners and returns 404. The client
	// translates that into an empty list so the orchestrator's zombie-runner
	// reaper does not flood the log on every poll.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	c := New(srv.URL, "orgs/x", "t")
	runners, err := c.ListRunners(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runners) != 0 {
		t.Fatalf("runners = %+v, want empty", runners)
	}
}

func TestDeleteRunner(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "orgs/example", "t")
	if err := c.DeleteRunner(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || !strings.HasSuffix(gotPath, "/actions/runners/7") {
		t.Errorf("DeleteRunner hit %s %s", gotMethod, gotPath)
	}
}

func TestDoNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(srv.URL, "orgs/x", "t")
	if _, err := c.WaitingJobs(context.Background()); err == nil {
		t.Fatal("expected error on 403")
	}
}
