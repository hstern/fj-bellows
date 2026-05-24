package linode

import (
	"mime"
	"mime/multipart"
	"strings"
	"testing"
)

func testValidWorkerExtras() workerExtrasData {
	return workerExtrasData{
		CACertPEM:    "-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n",
		CacheHost:    defaultCacheHostname,
		CacheIP:      "10.0.0.42",
		CachePort:    defaultCachePort,
		UpstreamHost: "upstream.example.com",
	}
}

func TestWrapWorkerUserDataForCacheProducesMergeableMIME(t *testing.T) {
	base := "#cloud-config\nruncmd:\n  - echo base\n"
	out, err := wrapWorkerUserDataForCache(base, testValidWorkerExtras())
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	// Must be parseable as multipart/mixed.
	mt, params, err := mime.ParseMediaType(strings.SplitN(out, "\r\n", 3)[1][len("Content-Type: "):])
	if err != nil {
		t.Fatalf("parse top-level Content-Type: %v", err)
	}
	if mt != "multipart/mixed" {
		t.Errorf("Content-Type = %q, want multipart/mixed", mt)
	}
	if params["boundary"] == "" {
		t.Fatal("multipart boundary missing")
	}
	// Walk the parts; each must be text/cloud-config.
	body := strings.SplitN(out, "\r\n\r\n", 2)[1]
	r := multipart.NewReader(strings.NewReader(body), params["boundary"])
	count := 0
	for {
		p, err := r.NextPart()
		if err != nil {
			break
		}
		if p.Header.Get("Content-Type") != "text/cloud-config" {
			t.Errorf("part %d Content-Type = %q, want text/cloud-config", count, p.Header.Get("Content-Type"))
		}
		count++
	}
	if count != 2 {
		t.Errorf("got %d parts, want 2 (base + extras)", count)
	}
}

func TestWrapWorkerUserDataForCachePropagatesBaseAndExtras(t *testing.T) {
	base := "#cloud-config\nruncmd:\n  - echo unique-base-marker\n"
	out, err := wrapWorkerUserDataForCache(base, testValidWorkerExtras())
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	for _, want := range []string{
		"unique-base-marker",                 // base content present
		"FAKECA",                             // CA PEM body present
		"10.0.0.42",                          // cache IP
		defaultCacheHostname,                 // cache hostname
		"upstream.example.com",               // upstream host
		"update-ca-certificates",             // runcmd from extras
		`capabilities = ["pull", "resolve"]`, // pull-only mirror
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wrapped output missing %q\n---\n%s", want, out)
		}
	}
	// Must NOT include "push" in capabilities — that's the boundary
	// that keeps push-to-upstream from being silently captured.
	if strings.Contains(out, `"push"`) {
		t.Errorf("worker mirror config contains \"push\" capability — boundary broken")
	}
}

func TestWrapWorkerUserDataForCacheRejectsEmptyBase(t *testing.T) {
	if _, err := wrapWorkerUserDataForCache("", testValidWorkerExtras()); err == nil {
		t.Error("expected error on empty base user-data")
	}
}

func TestRenderWorkerCacheExtrasRequiresAllFields(t *testing.T) {
	base := testValidWorkerExtras()
	cases := []struct {
		name string
		wipe func(*workerExtrasData)
	}{
		{name: "missing CA", wipe: func(x *workerExtrasData) { x.CACertPEM = "" }},
		{name: "missing host", wipe: func(x *workerExtrasData) { x.CacheHost = "" }},
		{name: "missing IP", wipe: func(x *workerExtrasData) { x.CacheIP = "" }},
		{name: "missing port", wipe: func(x *workerExtrasData) { x.CachePort = 0 }},
		{name: "missing upstream", wipe: func(x *workerExtrasData) { x.UpstreamHost = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := base
			c.wipe(&x)
			if _, err := renderWorkerCacheExtras(x); err == nil {
				t.Errorf("expected error for %q", c.name)
			}
		})
	}
}

func TestMultipartCloudInitSinglePartUnchanged(t *testing.T) {
	// One part = no wrap. The base cloud-init is passed through
	// verbatim, which keeps the no-cache path identical to PR 2a.
	out := multipartCloudInit([]string{"#cloud-config\nfoo: bar\n"})
	if out != "#cloud-config\nfoo: bar\n" {
		t.Errorf("single-part wrap should be a no-op, got: %q", out)
	}
}

func TestMultipartCloudInitZeroPartsEmpty(t *testing.T) {
	if out := multipartCloudInit(nil); out != "" {
		t.Errorf("zero parts should yield empty string, got: %q", out)
	}
}
