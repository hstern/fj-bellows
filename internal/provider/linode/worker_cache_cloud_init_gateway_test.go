package linode

import (
	"strings"
	"testing"
)

func testValidGatewayExtras() workerExtrasData {
	x := testValidWorkerExtras()
	x.TransportMode = transportCacheGateway
	x.TunnelRoutes = []string{"192.168.0.0/24", "10.10.0.0/16"}
	return x
}

// TestRenderWorkerCacheExtras_SSHKeepsHostsEntry — legacy SSH mode
// still emits the /etc/hosts cache entry (no DNS pointer, no tunnel
// routes). Pinning the legacy behavior so the new branch can't
// silently regress it.
func TestRenderWorkerCacheExtras_SSHKeepsHostsEntry(t *testing.T) {
	x := testValidWorkerExtras() // TransportMode default = "" → ssh
	got, err := renderWorkerCacheExtras(x)
	if err != nil {
		t.Fatalf("renderWorkerCacheExtras: %v", err)
	}
	wantSubstrings := []string{
		"path: /usr/local/share/ca-certificates/fjb-cache.crt",
		"path: /etc/docker/certs.d/" + defaultCacheHostname + ":",
		"path: /etc/hosts",
		"10.0.0.42 " + defaultCacheHostname,
		"- update-ca-certificates",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in SSH-mode extras:\n%s", sub, got)
		}
	}
	// SSH-mode template MUST NOT contain the new resolved.conf or
	// route plumbing.
	notWanted := []string{
		"resolved.conf.d",
		"ip route replace",
		"systemctl restart systemd-resolved",
	}
	for _, sub := range notWanted {
		if strings.Contains(got, sub) {
			t.Errorf("unexpected cache-gateway artifact %q in SSH-mode extras:\n%s", sub, got)
		}
	}
}

// TestRenderWorkerCacheExtras_CacheGatewayDropsHostsAddsDNSAndRoutes
// — cache-gateway mode picks the new template: keeps CA + cert.d,
// drops /etc/hosts entry, adds resolved.conf drop-in + ip route
// commands for each TunnelRoute.
func TestRenderWorkerCacheExtras_CacheGatewayDropsHostsAddsDNSAndRoutes(t *testing.T) {
	got, err := renderWorkerCacheExtras(testValidGatewayExtras())
	if err != nil {
		t.Fatalf("renderWorkerCacheExtras: %v", err)
	}
	// Kept: CA + cert.d injection.
	if !strings.Contains(got, "path: /usr/local/share/ca-certificates/fjb-cache.crt") {
		t.Errorf("CA file missing under cache-gateway:\n%s", got)
	}
	if !strings.Contains(got, "path: /etc/docker/certs.d/"+defaultCacheHostname+":") {
		t.Errorf("cert.d file missing under cache-gateway:\n%s", got)
	}
	if !strings.Contains(got, "- update-ca-certificates") {
		t.Errorf("update-ca-certificates runcmd missing under cache-gateway:\n%s", got)
	}
	// DROPPED: /etc/hosts cache entry.
	if strings.Contains(got, defaultCacheHostname+"\n") && strings.Contains(got, "10.0.0.42 "+defaultCacheHostname) {
		t.Errorf("unexpected /etc/hosts entry under cache-gateway:\n%s", got)
	}
	// ADDED: resolved.conf drop-in pointing at cache.
	if !strings.Contains(got, "path: /etc/systemd/resolved.conf.d/fjb-cache.conf") {
		t.Errorf("resolved.conf drop-in missing under cache-gateway:\n%s", got)
	}
	if !strings.Contains(got, "DNS=10.0.0.42") {
		t.Errorf("DNS= line not pointing at cache VPC IP:\n%s", got)
	}
	if !strings.Contains(got, "systemctl restart systemd-resolved") {
		t.Errorf("systemd-resolved restart missing:\n%s", got)
	}
	// ADDED: ip route for each TunnelRoute, via cache VPC IP.
	wantRoutes := []string{
		"ip route replace 192.168.0.0/24 via 10.0.0.42",
		"ip route replace 10.10.0.0/16 via 10.0.0.42",
	}
	for _, sub := range wantRoutes {
		if !strings.Contains(got, sub) {
			t.Errorf("missing route %q under cache-gateway:\n%s", sub, got)
		}
	}
}

// TestRenderWorkerCacheExtras_CacheGatewayNoRoutes — empty TunnelRoutes
// is valid; the template emits no route commands but everything else
// still renders.
func TestRenderWorkerCacheExtras_CacheGatewayNoRoutes(t *testing.T) {
	x := testValidGatewayExtras()
	x.TunnelRoutes = nil
	got, err := renderWorkerCacheExtras(x)
	if err != nil {
		t.Fatalf("renderWorkerCacheExtras: %v", err)
	}
	if strings.Contains(got, "ip route replace") {
		t.Errorf("unexpected route command when TunnelRoutes is empty:\n%s", got)
	}
	// DNS pointer + resolved.conf still present.
	if !strings.Contains(got, "DNS=10.0.0.42") {
		t.Errorf("DNS pointer missing without tunnel routes:\n%s", got)
	}
}

// TestWrapWorkerUserDataForCache_CacheGatewayProducesMergeable — full
// MIME-multipart wrap still produces a valid cloud-init multipart blob
// under cache-gateway mode. Smoke test: the merge wrapper is template-
// agnostic but worth confirming.
func TestWrapWorkerUserDataForCache_CacheGatewayProducesMergeable(t *testing.T) {
	base := "#cloud-config\nruncmd:\n  - echo base\n"
	out, err := wrapWorkerUserDataForCache(base, testValidGatewayExtras())
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	// Coarse checks: the wrapper still produces a multipart MIME with
	// both parts and the cache-gateway extras land in the second part.
	if !strings.Contains(out, "Content-Type: multipart/mixed") {
		t.Errorf("not multipart/mixed:\n%s", out)
	}
	if !strings.Contains(out, "ip route replace") {
		t.Errorf("cache-gateway route plumbing missing from wrapped output")
	}
	if !strings.Contains(out, "echo base") {
		t.Errorf("base user-data dropped during wrap")
	}
}
