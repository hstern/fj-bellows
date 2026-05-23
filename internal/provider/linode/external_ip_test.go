package linode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Stable URLs / values reused across the external-IP / GH-meta / firewall
// tests. goconst would otherwise flag the repetition.
const (
	testV4URL   = "https://ipv4.example"
	testV6URL   = "https://ipv6.example"
	testMetaURL = "https://meta.example"

	testCIDR1 = "192.0.2.10/32"
	testCIDR2 = "198.51.100.5/32"
	testTag   = "test-deploy"
)

// stubDoer returns a fixed (response, err) per URL. Hand-rolled — no httptest
// server needed for these tiny synthetic responses.
type stubDoer map[string]stubResp

type stubResp struct {
	body   string
	status int
	err    error
}

func (s stubDoer) Do(req *http.Request) (*http.Response, error) {
	r, ok := s[req.URL.String()]
	if !ok {
		return nil, errors.New("stubDoer: unexpected URL " + req.URL.String())
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return &http.Response{
		StatusCode: r.status,
		Body:       io.NopCloser(strings.NewReader(r.body)),
	}, nil
}

func TestResolveExternalIPBothFamilies(t *testing.T) {
	probe := externalIPProbe{
		v4URL: testV4URL,
		v6URL: testV6URL,
		client: stubDoer{
			testV4URL: {body: "203.0.113.10\n"},
			testV6URL: {body: "2001:db8::1\n"},
		},
	}
	got, err := resolveExternalIP(context.Background(), probe)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"203.0.113.10/32", "2001:db8::1/128"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveExternalIPOnlyV4(t *testing.T) {
	probe := externalIPProbe{
		v4URL: testV4URL,
		v6URL: testV6URL,
		client: stubDoer{
			testV4URL: {body: "203.0.113.10\n"},
			testV6URL: {err: errors.New("no v6")},
		},
	}
	got, err := resolveExternalIP(context.Background(), probe)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "203.0.113.10/32" {
		t.Errorf("got %v, want [203.0.113.10/32]", got)
	}
}

func TestResolveExternalIPOnlyV6(t *testing.T) {
	probe := externalIPProbe{
		v4URL: testV4URL,
		v6URL: testV6URL,
		client: stubDoer{
			testV4URL: {err: errors.New("no v4")},
			testV6URL: {body: "2001:db8::1\n"},
		},
	}
	got, err := resolveExternalIP(context.Background(), probe)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "2001:db8::1/128" {
		t.Errorf("got %v, want [2001:db8::1/128]", got)
	}
}

func TestResolveExternalIPBothFail(t *testing.T) {
	probe := externalIPProbe{
		v4URL: testV4URL,
		v6URL: testV6URL,
		client: stubDoer{
			testV4URL: {err: errors.New("v4 down")},
			testV6URL: {err: errors.New("v6 down")},
		},
	}
	if _, err := resolveExternalIP(context.Background(), probe); err == nil {
		t.Fatal("want error when both probes fail")
	}
}

func TestResolveExternalIPRejectsWrongFamily(t *testing.T) {
	// v4 endpoint returning a v6 literal is treated as failure (family mismatch).
	probe := externalIPProbe{
		v4URL: testV4URL,
		v6URL: testV6URL,
		client: stubDoer{
			testV4URL: {body: "2001:db8::1\n"},
			testV6URL: {body: "203.0.113.10\n"},
		},
	}
	if _, err := resolveExternalIP(context.Background(), probe); err == nil {
		t.Fatal("want error when both endpoints return the wrong family")
	}
}

func TestResolveExternalIPRejectsGarbage(t *testing.T) {
	probe := externalIPProbe{
		v4URL: testV4URL,
		v6URL: testV6URL,
		client: stubDoer{
			testV4URL: {body: "not-an-ip"},
			testV6URL: {body: "also-not-an-ip"},
		},
	}
	if _, err := resolveExternalIP(context.Background(), probe); err == nil {
		t.Fatal("want error when responses aren't IP literals")
	}
}
