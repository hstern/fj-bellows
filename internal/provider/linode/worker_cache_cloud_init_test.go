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
		"/etc/docker/daemon.json",            // FJB-9: dockerd snapshotter config path
		`{"features": {"containerd-snapshotter": true}}`, // FJB-9: feature payload
		"systemctl restart docker",                       // FJB-9: pick up daemon.json
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

// TestWrapWorkerUserDataForCacheRoutesDockerThroughContainerd is the
// FJB-9 regression: without the containerd-snapshotter daemon.json,
// dockerd ignores /etc/containerd/certs.d/ and every job-container
// pull bypasses the cache (zero objects in the cache S3 bucket).
// Three load-bearing facts on the wire:
//  1. The daemon.json path is correct (dockerd reads exactly this path).
//  2. The feature payload is exactly the keys/values dockerd expects.
//  3. A docker restart fires so daemon.json is picked up even when
//     dockerd was running before /etc/docker/daemon.json existed.
func TestWrapWorkerUserDataForCacheRoutesDockerThroughContainerd(t *testing.T) {
	out, err := wrapWorkerUserDataForCache("#cloud-config\n", testValidWorkerExtras())
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if !strings.Contains(out, "path: /etc/docker/daemon.json") {
		t.Errorf("daemon.json write_files entry missing — dockerd will keep its own image store:\n%s", out)
	}
	if !strings.Contains(out, `{"features": {"containerd-snapshotter": true}}`) {
		t.Errorf("daemon.json feature payload missing or wrong:\n%s", out)
	}
	if !strings.Contains(out, "systemctl restart docker") {
		t.Errorf("docker restart missing — daemon.json isn't hot-reloaded so the running dockerd won't pick it up:\n%s", out)
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
