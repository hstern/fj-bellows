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

func TestWaitingJobsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/actions/runners/jobs") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token secret" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = io.WriteString(w, `{"jobs":[{"id":1,"handle":"h1","labels":["ubuntu-latest"]}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "orgs/example", "secret")
	jobs, err := c.WaitingJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Handle != "h1" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestWaitingJobsBareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":2,"handle":"h2"}]`)
	}))
	defer srv.Close()

	c := New(srv.URL, "repos/o/r", "t")
	jobs, err := c.WaitingJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Handle != "h2" {
		t.Fatalf("jobs = %+v", jobs)
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

	c := New(srv.URL, "orgs/example", "t")
	reg, err := c.RegisterEphemeral(context.Background(), "runner-x", []string{"ubuntu-latest"})
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
