package linode

import (
	"context"
	"errors"
	"testing"
)

func TestFetchGithubActionsCIDRsHappyPath(t *testing.T) {
	client := stubDoer{
		testMetaURL: {body: `{"actions":["192.0.2.0/24","2001:db8::/32","198.51.100.10/32"]}`},
	}
	got, err := fetchGithubActionsCIDRs(context.Background(), client, testMetaURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"192.0.2.0/24", "2001:db8::/32", "198.51.100.10/32"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestFetchGithubActionsCIDRsDropsMalformed(t *testing.T) {
	client := stubDoer{
		testMetaURL: {body: `{"actions":["192.0.2.0/24","not-a-cidr","2001:db8::/32"]}`},
	}
	got, err := fetchGithubActionsCIDRs(context.Background(), client, testMetaURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 valid CIDRs, got %d: %v", len(got), got)
	}
}

func TestFetchGithubActionsCIDRsMalformedJSON(t *testing.T) {
	client := stubDoer{
		testMetaURL: {body: `{not json`},
	}
	if _, err := fetchGithubActionsCIDRs(context.Background(), client, testMetaURL); err == nil {
		t.Fatal("want error on malformed JSON")
	}
}

func TestFetchGithubActionsCIDRsHTTPError(t *testing.T) {
	client := stubDoer{
		testMetaURL: {err: errors.New("network down")},
	}
	if _, err := fetchGithubActionsCIDRs(context.Background(), client, testMetaURL); err == nil {
		t.Fatal("want error on network failure")
	}
}

func TestFetchGithubActionsCIDRsEmptyActions(t *testing.T) {
	// .actions key present but empty (or all malformed) -> degenerate, error.
	client := stubDoer{
		testMetaURL: {body: `{"actions":[]}`},
	}
	if _, err := fetchGithubActionsCIDRs(context.Background(), client, testMetaURL); err == nil {
		t.Fatal("want error on empty actions list")
	}
}

func TestFetchGithubActionsCIDRsNon200(t *testing.T) {
	client := stubDoer{
		testMetaURL: {status: 503, body: "boom"},
	}
	if _, err := fetchGithubActionsCIDRs(context.Background(), client, testMetaURL); err == nil {
		t.Fatal("want error on non-200 status")
	}
}
