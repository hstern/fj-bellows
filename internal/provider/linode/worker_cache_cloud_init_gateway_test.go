package linode

import (
	"strings"
	"testing"
)

// testValidGatewayExtras returns a workerExtrasData valid for the
// cache-gateway template — ACL-derived AllowedIPsCIDRs + the default
// orchestrator WG address.
func testValidGatewayExtras() workerExtrasData {
	x := testValidWorkerExtras()
	x.TransportMode = transportCacheGateway
	x.AllowedIPsCIDRs = []string{testCIDRLan192, testCIDRLan10}
	x.OrchestratorWGAddr = defaultOrchestratorWGAddr
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
	// SSH-mode template MUST NOT contain the new resolv.conf or
	// route plumbing.
	notWanted := []string{
		"resolv.conf",
		"ip route replace",
		"nameserver 100.64.0.1",
	}
	for _, sub := range notWanted {
		if strings.Contains(got, sub) {
			t.Errorf("unexpected cache-gateway artifact %q in SSH-mode extras:\n%s", sub, got)
		}
	}
}

// TestRenderWorkerCacheExtras_CacheGatewayResolvConfAndRoutes pins the
// FJB-88 shape: cache-gateway mode emits exactly one /etc/resolv.conf
// nameserver line pointing at the orchestrator's WG address, one
// `ip route replace` per AllowedIPsCIDR, and keeps CA + cert.d
// injection unchanged.
func TestRenderWorkerCacheExtras_CacheGatewayResolvConfAndRoutes(t *testing.T) {
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
	// DROPPED: /etc/hosts cache entry + legacy systemd-resolved drop-in.
	if strings.Contains(got, "path: /etc/hosts") {
		t.Errorf("unexpected /etc/hosts write_files entry under cache-gateway:\n%s", got)
	}
	if strings.Contains(got, "systemd/resolved.conf.d") {
		t.Errorf("legacy resolved.conf.d drop-in must be gone:\n%s", got)
	}
	if strings.Contains(got, "systemctl restart systemd-resolved") {
		t.Errorf("legacy systemd-resolved restart must be gone:\n%s", got)
	}

	// ADDED: /etc/resolv.conf with single nameserver line.
	if !strings.Contains(got, "path: /etc/resolv.conf") {
		t.Errorf("resolv.conf write_files entry missing:\n%s", got)
	}
	nameserverLine := "nameserver " + defaultOrchestratorWGAddr
	if c := strings.Count(got, nameserverLine); c != 1 {
		t.Errorf("expected exactly 1 %q line, got %d:\n%s", nameserverLine, c, got)
	}

	// ADDED: ip route for each AllowedIPsCIDR, via cache VPC IP.
	wantRoutes := []string{
		"ip route replace 192.168.0.0/24 via 10.0.0.42",
		"ip route replace 10.10.0.0/16 via 10.0.0.42",
	}
	for _, sub := range wantRoutes {
		if !strings.Contains(got, sub) {
			t.Errorf("missing route %q under cache-gateway:\n%s", sub, got)
		}
	}
	// One `ip route replace` line per CIDR, no more, no less.
	if c := strings.Count(got, "ip route replace"); c != len(wantRoutes) {
		t.Errorf("expected %d `ip route replace` lines, got %d:\n%s", len(wantRoutes), c, got)
	}
}

// TestRenderWorkerCacheExtras_CacheGatewayRejectsEmptyAllowedIPs —
// FJB-88 validation: a cache-gateway worker with zero AllowedIPs CIDRs
// has no path to reach anything across WG. Refuse rather than
// silently provision a broken worker.
func TestRenderWorkerCacheExtras_CacheGatewayRejectsEmptyAllowedIPs(t *testing.T) {
	x := testValidGatewayExtras()
	x.AllowedIPsCIDRs = nil
	_, err := renderWorkerCacheExtras(x)
	if err == nil {
		t.Fatal("expected error when AllowedIPsCIDRs is empty under cache-gateway")
	}
	if !strings.Contains(err.Error(), "AllowedIPsCIDRs") {
		t.Errorf("error should mention AllowedIPsCIDRs, got: %v", err)
	}
}

// TestRenderWorkerCacheExtras_CacheGatewayRejectsEmptyOrchestratorAddr —
// FJB-88 validation: without an OrchestratorWGAddr the resolv.conf
// line would render as `nameserver ` and break DNS on the worker.
func TestRenderWorkerCacheExtras_CacheGatewayRejectsEmptyOrchestratorAddr(t *testing.T) {
	x := testValidGatewayExtras()
	x.OrchestratorWGAddr = ""
	_, err := renderWorkerCacheExtras(x)
	if err == nil {
		t.Fatal("expected error when OrchestratorWGAddr is empty under cache-gateway")
	}
	if !strings.Contains(err.Error(), "OrchestratorWGAddr") {
		t.Errorf("error should mention OrchestratorWGAddr, got: %v", err)
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
	if !strings.Contains(out, "nameserver "+defaultOrchestratorWGAddr) {
		t.Errorf("resolv.conf nameserver line missing from wrapped output")
	}
	if !strings.Contains(out, "echo base") {
		t.Errorf("base user-data dropped during wrap")
	}
}
