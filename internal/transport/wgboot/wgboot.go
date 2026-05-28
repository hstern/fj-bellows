// Package wgboot wires the orchestrator's cache-gateway WireGuard
// stack together at startup (FJB-90). It pulls the per-package
// libraries landed in FJB-78..FJB-86 — tunnel, ACL parser + resolver,
// DNS responder, transparent TCP proxy, UDP forwarder, ICMP echo
// bridge — and stitches them into one Stack that the orchestrator's
// main.go starts and stops as a unit.
//
// The package's responsibilities:
//
//  1. Load (or generate) the WG private key from the configured path
//     and log the public key once at INFO so the operator can paste it
//     into the cache nanode's wg-quick config on first run.
//  2. Construct the embedded Tunnel and bring it up.
//  3. Parse the ACL (operator entries + implicit forgejo + DNS-bind
//     entries via acl.ImplicitEntries + acl.DedupeRaw) and build the
//     Registry.
//  4. Start the per-host DNS Resolver loop.
//  5. Bring up the DNS responder bound on the orchestrator's WG
//     overlay address (default 100.64.0.1:53), seeded with `cache` →
//     cache VPC IP via the InternalTable.
//  6. Bring up the transparent TCP proxy, the UDP forwarder, and the
//     ICMP echo bridge — each gated by the registry's Snapshot.Lookup.
//  7. Wire the Linode provider's ACL source so worker cloud-init's
//     `ip route replace` lines reflect the live AllowedIPs set.
//  8. Subscribe to ACL Registry changes and re-render the cache's
//     wg-quick + iptables config on every change. v1 logs the
//     rendered content + TODO; pushing to the cache nanode is a
//     follow-up.
//
// Stack.Close orchestrates shutdown in the order proxy → udp forwarder
// → icmp bridge → dns responder → ACL resolver → tunnel so nothing
// blocks waiting on a closed downstream.
package wgboot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"

	xicmp "golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/hstern/fj-bellows/internal/config"
	"github.com/hstern/fj-bellows/internal/transport/cachegateway"
	"github.com/hstern/fj-bellows/internal/transport/wg"
	"github.com/hstern/fj-bellows/internal/transport/wg/acl"
	"github.com/hstern/fj-bellows/internal/transport/wg/dns"
	"github.com/hstern/fj-bellows/internal/transport/wg/icmp"
	"github.com/hstern/fj-bellows/internal/transport/wg/udp"
)

// ACLSource is the narrow read-only view the Linode provider needs.
// Re-declared here (rather than imported from internal/provider/linode)
// so this package doesn't take a provider dep. The Linode provider's
// ACLSnapshotSource has the same shape — main.go assigns one to the
// other at the call site.
type ACLSource interface {
	AllowedIPsCIDRs() []string
}

// CacheRenderInputs collects the deployment-side facts the
// cache-config re-render needs that the Tunnel + Registry don't carry
// themselves. Populated by main.go from the live provider + config.
type CacheRenderInputs struct {
	// CacheVPCIP is the cache nanode's VPC-side IPv4. Used as the
	// iptables FORWARD destination glue and the dns InternalTable
	// seed for `cache`.
	CacheVPCIP string

	// WorkerVPCSubnet is the worker VPC CIDR ("10.0.0.0/24" etc.).
	// Drives the iptables FORWARD source filter on the cache.
	WorkerVPCSubnet string

	// CachePrivateKey is the cache nanode's WG private key (base64).
	// In v1 we don't actually push the rendered wg0.conf — the
	// operator pastes it. Empty disables the wg-quick render (we
	// still render the iptables script).
	CachePrivateKey string
}

// Config is the input bundle for Boot.
type Config struct {
	// Transport is the validated transport config block (Mode must
	// be cache-gateway; WG + Tunnel must be non-nil).
	Transport config.Transport

	// ForgejoURL is the deployment's Forgejo base URL, used to seed
	// the implicit ACL entry tcp://<forgejo-host>:<port>.
	ForgejoURL string

	// ACLSink receives the registry adapter so worker cloud-init can
	// read the live AllowedIPs at Provision time (FJB-88). The
	// Linode provider's SetACLSource consumes this. May be nil for
	// providers that don't carry a managed cache.
	ACLSink func(ACLSource)

	// Cache supplies the cache-side facts the renderer needs. Pass
	// the cache nanode's VPC IP, worker VPC subnet, and (when
	// available) the cache's WG private key. CacheVPCIP MUST be
	// non-empty (the DNS table seeds `cache` → CacheVPCIP).
	Cache CacheRenderInputs

	// Logger receives boot + runtime events. Falls back to
	// slog.Default when nil.
	Logger *slog.Logger
}

// Stack is the running cache-gateway transport. Returned by Boot,
// closed at shutdown. The fields are read-only after construction;
// callers must not mutate them.
type Stack struct {
	Tunnel    *wg.Tunnel
	Registry  *acl.Registry
	Resolver  *acl.Resolver
	Responder *dns.Responder
	Proxy     *wg.Proxy
	UDP       *udp.Forwarder
	ICMP      *icmp.Bridge

	// PublicKey is the orchestrator's WG public key, logged at INFO
	// during Boot for first-run operator paste-in.
	PublicKey string

	log *slog.Logger

	// shutdown closes everything in the right order; built up during
	// Boot so partial-failure cleanup still calls every Close.
	shutdownMu sync.Mutex
	shutdown   []func()
	closed     bool

	// rerender stops the cache-config re-render watcher started by
	// startCacheRenderWatcher; nil when no cache render inputs were
	// provided.
	rerenderUnsub func()
}

// Boot wires everything together. On success the returned Stack is
// running and Close-able. On failure every partial resource is torn
// down before returning the error.
//
// Failures categorized:
//
//   - Private key unreadable → wrapped error returned (callers exit 1).
//   - ACL parse fails → wrapped error returned.
//   - DNS responder bind fails → wrapped error returned.
//   - WG handshake doesn't complete on first attempt: not an error.
//     Keepalive will retry; logged at WARN by the device's own logger.
func Boot(ctx context.Context, cfg Config) (*Stack, error) {
	if err := validateBootInputs(cfg); err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	plan, err := planBoot(cfg, log)
	if err != nil {
		return nil, err
	}

	s := &Stack{log: log, PublicKey: plan.publicKey}
	if err := startStack(ctx, s, cfg, plan); err != nil {
		s.closeAllLocked()
		return nil, err
	}
	// Wire ACL sink + cache-render watcher after every component is up,
	// so a failed start doesn't leave the provider holding an
	// adapter that points at a torn-down registry.
	if cfg.ACLSink != nil {
		cfg.ACLSink(&registryAdapter{reg: s.Registry, excludeCache: plan.cacheIP})
	}
	s.startCacheRenderWatcher(ctx, cfg, plan.overlayPrefix, plan.localAddr)

	log.Info("wgboot: cache-gateway transport up",
		"local_addr", plan.localAddr.String(),
		"overlay_prefix", plan.overlayPrefix.String(),
		"peer_endpoint", plan.endpoint.String(),
		"acl_entries", len(plan.entries),
		"dns_addr", net.JoinHostPort(plan.localAddr.String(), "53"),
	)
	return s, nil
}

// validateBootInputs encapsulates the cheap syntactic checks Boot does
// before touching any I/O, so the entry point stays under the
// cyclomatic budget.
func validateBootInputs(cfg Config) error {
	if cfg.Transport.Mode != config.TransportCacheGateway {
		return fmt.Errorf("wgboot: transport.mode = %q; want %q",
			cfg.Transport.Mode, config.TransportCacheGateway)
	}
	if cfg.Transport.WG == nil {
		return errors.New("wgboot: transport.wg is required under cache-gateway mode")
	}
	if cfg.ForgejoURL == "" {
		return errors.New("wgboot: ForgejoURL is required")
	}
	if cfg.Cache.CacheVPCIP == "" {
		return errors.New("wgboot: Cache.CacheVPCIP is required")
	}
	return nil
}

// bootPlan collects the pre-component values Boot needs to bring up
// the stack. Splitting parsing out of component startup keeps Boot's
// cyclomatic complexity manageable + lets tests target plan errors
// without touching the netstack.
type bootPlan struct {
	priv          wgtypes.Key
	publicKey     string
	overlayPrefix netip.Prefix
	localAddr     netip.Addr
	endpoint      *net.UDPAddr
	peerKey       wgtypes.Key
	entries       []acl.Entry
	peerAllowed   []netip.Prefix
	cacheIP       netip.Addr
}

func planBoot(cfg Config, log *slog.Logger) (*bootPlan, error) {
	priv, err := wg.LoadOrGenerateKey(cfg.Transport.WG.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("wgboot: load private key: %w", err)
	}
	pub := priv.PublicKey()
	log.Info("wgboot: orchestrator wg public key (paste into cache wg0.conf [Peer].PublicKey)",
		"public_key", pub.String(),
		"private_key_file", cfg.Transport.WG.PrivateKeyFile,
	)

	overlayPrefix, err := netip.ParsePrefix(cfg.Transport.WG.OverlayPrefix)
	if err != nil {
		return nil, fmt.Errorf("wgboot: parse overlay_prefix %q: %w", cfg.Transport.WG.OverlayPrefix, err)
	}
	localAddr, err := parseHostAddr(cfg.Transport.WG.LocalAddr)
	if err != nil {
		return nil, fmt.Errorf("wgboot: parse local_addr %q: %w", cfg.Transport.WG.LocalAddr, err)
	}
	endpoint, err := net.ResolveUDPAddr("udp", cfg.Transport.WG.Peer.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("wgboot: resolve peer endpoint %q: %w", cfg.Transport.WG.Peer.Endpoint, err)
	}
	peerKey, err := wg.DecodeKey(cfg.Transport.WG.Peer.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("wgboot: parse peer public_key: %w", err)
	}

	entries, err := parseACL(cfg, localAddr)
	if err != nil {
		return nil, err
	}

	peerAllowed, err := composePeerAllowedIPs(overlayPrefix, cfg.Transport.WG.Peer.AllowedIPs, cfg.Cache.WorkerVPCSubnet)
	if err != nil {
		return nil, err
	}

	cacheIP, err := netip.ParseAddr(cfg.Cache.CacheVPCIP)
	if err != nil {
		return nil, fmt.Errorf("wgboot: parse cache VPC IP %q: %w", cfg.Cache.CacheVPCIP, err)
	}

	return &bootPlan{
		priv:          priv,
		publicKey:     pub.String(),
		overlayPrefix: overlayPrefix,
		localAddr:     localAddr,
		endpoint:      endpoint,
		peerKey:       peerKey,
		entries:       entries,
		peerAllowed:   peerAllowed,
		cacheIP:       cacheIP,
	}, nil
}

func parseACL(cfg Config, localAddr netip.Addr) ([]acl.Entry, error) {
	implicit, err := acl.ImplicitEntries(cfg.ForgejoURL, localAddr)
	if err != nil {
		return nil, fmt.Errorf("wgboot: synthesize implicit ACL entries: %w", err)
	}
	_, entries, err := acl.DedupeRaw(cfg.Transport.WG.ACL, implicit)
	if err != nil {
		return nil, fmt.Errorf("wgboot: parse ACL: %w", err)
	}
	for _, e := range entries {
		if e.HasUnsupportedICMPTypes() {
			return nil, fmt.Errorf("wgboot: ACL entry %q has ICMP types outside {0, 8} (v1 supports echo only)", e.Original)
		}
	}
	return entries, nil
}

// composePeerAllowedIPs builds the orchestrator-side peer AllowedIPs:
// the overlay CIDR (host route to the cache), plus any operator-supplied
// extras, plus the worker VPC CIDR when known (FJB-92). The worker VPC
// CIDR is what makes the netstack encapsulate outbound packets the
// dispatcher dials at 10.0.0.X — without it, those addresses are
// invisible to the netstack and the dial fails with "no route".
func composePeerAllowedIPs(overlay netip.Prefix, raw []string, workerVPC string) ([]netip.Prefix, error) {
	out := []netip.Prefix{overlay}
	if workerVPC != "" {
		pfx, err := netip.ParsePrefix(workerVPC)
		if err != nil {
			return nil, fmt.Errorf("wgboot: parse worker VPC CIDR %q: %w", workerVPC, err)
		}
		if !slices.Contains(out, pfx) {
			out = append(out, pfx)
		}
	}
	for _, r := range raw {
		pfx, err := netip.ParsePrefix(r)
		if err != nil {
			return nil, fmt.Errorf("wgboot: parse peer allowed_ip %q: %w", r, err)
		}
		if !slices.Contains(out, pfx) {
			out = append(out, pfx)
		}
	}
	return out, nil
}

// startStack brings up every runtime component in order. Each step
// records a Close func on s so partial-startup failures unwind cleanly.
func startStack(ctx context.Context, s *Stack, cfg Config, plan *bootPlan) error {
	if err := startTunnel(s, cfg, plan); err != nil {
		return err
	}
	registry := acl.NewRegistry(plan.entries)
	s.Registry = registry

	if err := startResolver(ctx, s, registry); err != nil {
		return err
	}
	if err := startResponder(ctx, s, cfg, plan); err != nil {
		return err
	}
	if err := startProxy(ctx, s, registry); err != nil {
		return err
	}
	if err := startUDP(s, plan, registry); err != nil {
		return err
	}
	return startICMP(s, plan, registry)
}

func startTunnel(s *Stack, cfg Config, plan *bootPlan) error {
	tun, err := wg.New(wg.Config{
		PrivateKey:        plan.priv,
		LocalAddr:         plan.localAddr,
		ListenPort:        cfg.Transport.WG.ListenPort,
		KeepaliveInterval: cfg.Transport.WG.KeepaliveInterval.D(),
		Peer: wg.Peer{
			PublicKey:  plan.peerKey,
			Endpoint:   plan.endpoint,
			AllowedIPs: plan.peerAllowed,
		},
		Logger: s.log,
	})
	if err != nil {
		return fmt.Errorf("wgboot: bring up wg tunnel: %w", err)
	}
	s.Tunnel = tun
	s.pushShutdown(func() { _ = tun.Close() })
	return nil
}

func startResolver(ctx context.Context, s *Stack, registry *acl.Registry) error {
	r := acl.NewResolver(registry, newRealLookup())
	if err := r.Start(ctx); err != nil {
		return fmt.Errorf("wgboot: start ACL resolver: %w", err)
	}
	s.Resolver = r
	s.pushShutdown(r.Close)
	return nil
}

func startResponder(ctx context.Context, s *Stack, cfg Config, plan *bootPlan) error {
	table := dns.MapTable{"cache": plan.cacheIP}
	resp, err := dns.New(dns.Config{
		Listener:    s.Tunnel.DNSListener(),
		Table:       table,
		ListenAddr:  net.JoinHostPort(plan.localAddr.String(), "53"),
		Logger:      s.log,
		InternalTTL: dns.DefaultInternalTTL,
	})
	if err != nil {
		return fmt.Errorf("wgboot: construct DNS responder: %w", err)
	}
	if err := resp.Start(ctx); err != nil {
		return fmt.Errorf("wgboot: start DNS responder: %w", err)
	}
	_ = cfg
	s.Responder = resp
	s.pushShutdown(func() { _ = resp.Close() })
	return nil
}

func startProxy(ctx context.Context, s *Stack, registry *acl.Registry) error {
	p, err := wg.NewProxy(s.Tunnel, registry, s.log)
	if err != nil {
		return fmt.Errorf("wgboot: construct TCP proxy: %w", err)
	}
	if err := p.Start(ctx); err != nil {
		_ = p.Close()
		return fmt.Errorf("wgboot: start TCP proxy: %w", err)
	}
	s.Proxy = p
	s.pushShutdown(func() { _ = p.Close() })
	return nil
}

func startUDP(s *Stack, plan *bootPlan, registry *acl.Registry) error {
	fwd, err := startUDPForwarder(s.Tunnel, plan.localAddr, registry, s.log)
	if err != nil {
		return fmt.Errorf("wgboot: start UDP forwarder: %w", err)
	}
	s.UDP = fwd
	s.pushShutdown(func() { _ = fwd.Close() })
	return nil
}

func startICMP(s *Stack, plan *bootPlan, registry *acl.Registry) error {
	br, err := startICMPBridge(s.Tunnel, plan.localAddr, registry, s.log)
	if err != nil {
		return fmt.Errorf("wgboot: start ICMP bridge: %w", err)
	}
	s.ICMP = br
	s.pushShutdown(func() { _ = br.Close() })
	return nil
}

// Close tears down the stack in reverse construction order. Idempotent.
func (s *Stack) Close() error {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.rerenderUnsub != nil {
		s.rerenderUnsub()
	}
	// Walk in reverse so dependents close before their dependencies.
	for _, f := range slices.Backward(s.shutdown) {
		f()
	}
	return nil
}

// closeAllLocked is the partial-construction unwind path. Same logic
// as Close but doesn't take the lock — only safe from Boot's failure
// branches (single-threaded).
func (s *Stack) closeAllLocked() {
	if s.closed {
		return
	}
	s.closed = true
	if s.rerenderUnsub != nil {
		s.rerenderUnsub()
	}
	for _, f := range slices.Backward(s.shutdown) {
		f()
	}
}

func (s *Stack) pushShutdown(f func()) {
	s.shutdownMu.Lock()
	s.shutdown = append(s.shutdown, f)
	s.shutdownMu.Unlock()
}

// startCacheRenderWatcher fires once at start and again on every ACL
// Registry change to re-render the cache nanode's wg0.conf +
// iptables.sh. v1 logs the content at INFO; pushing the files to the
// cache is a follow-up.
func (s *Stack) startCacheRenderWatcher(ctx context.Context, cfg Config, overlay netip.Prefix, orchAddr netip.Addr) {
	cacheAddr := nextHostInPrefix(overlay, orchAddr)
	ch, unsub := s.Registry.Subscribe()
	s.rerenderUnsub = unsub
	render := func() {
		snap := s.Registry.Snapshot()
		prefixes := withOrchestratorHost(snap.Prefixes, orchAddr)

		iptablesIn := cachegateway.Inputs{
			CacheVPCIP:         cfg.Cache.CacheVPCIP,
			WorkerVPCSubnet:    cfg.Cache.WorkerVPCSubnet,
			AllowedIPs:         prefixes,
			OverlayPrefix:      overlay,
			OrchestratorWGAddr: orchAddr,
			CacheWGAddr:        cacheAddr,
		}
		// iptables render is unconditional — we always know enough to
		// produce it. Empty WorkerVPCSubnet falls through to the
		// renderer's validate() error.
		if iptablesIn.WorkerVPCSubnet == "" {
			s.log.Debug("wgboot: skipping iptables render — worker VPC subnet unknown",
				"acl_prefixes", len(prefixes))
		} else if iptables, err := cachegateway.RenderCacheIPTables(iptablesIn); err != nil {
			s.log.Warn("wgboot: render cache iptables", "err", err)
		} else {
			s.log.Info(
				"wgboot: cache iptables re-rendered (TODO: push to cache nanode via SSH; FJB-87/FJB-91)",
				"acl_prefixes", len(prefixes),
				"bytes", len(iptables),
			)
			s.log.Debug("wgboot: cache iptables content", "script", iptables)
		}

		if cfg.Cache.CachePrivateKey != "" {
			wgIn := cachegateway.WGInputs{
				CachePrivateKey:       cfg.Cache.CachePrivateKey,
				CacheWGAddr:           cacheAddr,
				ListenPort:            cfg.Transport.WG.ListenPort,
				OrchestratorPublicKey: s.PublicKey,
				AllowedIPs:            prefixes,
			}
			if wg0, err := cachegateway.RenderWGQuick(wgIn); err != nil {
				s.log.Warn("wgboot: render cache wg0.conf", "err", err)
			} else {
				s.log.Info(
					"wgboot: cache wg0.conf re-rendered (TODO: push to cache nanode via SSH; FJB-87/FJB-91)",
					"acl_prefixes", len(prefixes),
					"bytes", len(wg0),
				)
				s.log.Debug("wgboot: cache wg0.conf content", "config", wg0)
			}
		}
	}

	// First render immediately so the operator sees the initial
	// content; subsequent renders are change-driven.
	render()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}
				render()
			}
		}
	}()
}

// startProxyFromSpecs removed: FJB-85 replaced the per-spec proxy with
// an ACL-driven wildcard listener. The boot path now calls
// wg.NewProxy(tun, registry, log) directly; see startProxy.

// startUDPForwarder binds the netstack-side wildcard UDP listener and
// wires the registry-driven LookupFn.
func startUDPForwarder(tun *wg.Tunnel, localAddr netip.Addr, registry *acl.Registry, log *slog.Logger) (*udp.Forwarder, error) {
	pc, err := tun.ListenUDPAddrPort(netip.AddrPortFrom(localAddr, 0))
	if err != nil {
		return nil, fmt.Errorf("listen UDP wildcard: %w", err)
	}
	lookup := func(dst netip.Addr, port int) (string, bool) {
		entry := registry.Snapshot().Lookup(dst, port, acl.SchemeUDP)
		if entry == nil {
			return "", false
		}
		// Upstream addr: keep the literal host the operator wrote when
		// possible (so DNS-based entries reach the resolved IP family
		// the operator picked), otherwise the canonical IP literal.
		host := entry.Host
		if entry.IsDomain() {
			host = dst.String()
		}
		return net.JoinHostPort(host, strconv.Itoa(port)), true
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{}
		return d.DialContext(ctx, network, addr)
	}
	fwd, err := udp.NewForwarder(pc, dial, lookup, log)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	fwd.Start()
	return fwd, nil
}

// startICMPBridge binds the netstack-side ICMP listener and wires the
// registry-driven LookupFn and an unprivileged ICMP originator
// (golang.org/x/net/icmp on Linux's ipproto-icmp DGRAM socket).
func startICMPBridge(tun *wg.Tunnel, localAddr netip.Addr, registry *acl.Registry, log *slog.Logger) (*icmp.Bridge, error) {
	in, err := tun.ListenPingAddr(localAddr)
	if err != nil {
		return nil, fmt.Errorf("listen ICMP: %w", err)
	}
	lookup := func(dst netip.Addr) (string, bool) {
		entry := registry.Snapshot().Lookup(dst, 0, acl.SchemeICMP)
		if entry == nil {
			return "", false
		}
		host := entry.Host
		if entry.IsDomain() {
			host = dst.String()
		}
		return host, true
	}
	br, err := icmp.NewBridge(in, originateICMP, lookup, log)
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	br.Start()
	return br, nil
}

// originateICMP sends one echo request to upstream via the unprivileged
// ICMP DGRAM socket on Linux and returns the reply payload.
//
// Linux's unprivileged ICMP socket (proto 1, IPPROTO_ICMP via SOCK_DGRAM)
// stamps its own id field — the kernel rewrites it to the source-port
// equivalent and re-rewrites on receive — so we don't pass id through
// the wire. The bridge mirrors the inbound id back to the requester
// itself (see icmp.Bridge.dispatch).
func originateICMP(ctx context.Context, upstream string, _, seq int, data []byte) ([]byte, int, int, error) {
	conn, err := xicmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return nil, 0, 0, fmt.Errorf("listen icmp udp4: %w", err)
	}
	defer func() { _ = conn.Close() }()

	dst, err := net.ResolveIPAddr("ip4", upstream)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("resolve %q: %w", upstream, err)
	}
	deadline, ok := ctx.Deadline()
	if ok {
		_ = conn.SetDeadline(deadline)
	}

	msg := &xicmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &xicmp.Echo{ID: 0, Seq: seq, Data: data},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("marshal echo: %w", err)
	}
	if _, err := conn.WriteTo(wire, &net.UDPAddr{IP: dst.IP}); err != nil {
		return nil, 0, 0, fmt.Errorf("write echo to %s: %w", upstream, err)
	}

	buf := make([]byte, 1500)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("read reply: %w", err)
	}
	reply, err := xicmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), buf[:n])
	if err != nil {
		return nil, 0, 0, fmt.Errorf("parse reply: %w", err)
	}
	echo, ok := reply.Body.(*xicmp.Echo)
	if !ok {
		return nil, 0, 0, fmt.Errorf("unexpected reply body %T", reply.Body)
	}
	return echo.Data, echo.ID, echo.Seq, nil
}

// registryAdapter implements the linode provider's ACLSnapshotSource:
// the de-duplicated, sorted set of CIDR strings the worker should
// route via the cache. Excludes the cache's own VPC /32 per
// transport.md "Worker route derivation".
type registryAdapter struct {
	reg          *acl.Registry
	excludeCache netip.Addr
}

// AllowedIPsCIDRs returns the registry's current Snapshot prefixes as
// canonical CIDR strings. The registry already sorts + dedupes; we
// just stringify, drop the cache's own /32 (workers reach the cache
// via the VPC, not WG), and return.
func (a *registryAdapter) AllowedIPsCIDRs() []string {
	snap := a.reg.Snapshot()
	out := make([]string, 0, len(snap.Prefixes))
	cacheHostBits := 32
	if a.excludeCache.Is6() && !a.excludeCache.Is4In6() {
		cacheHostBits = 128
	}
	cacheHost := netip.PrefixFrom(a.excludeCache, cacheHostBits)
	for _, p := range snap.Prefixes {
		if p == cacheHost {
			continue
		}
		out = append(out, p.String())
	}
	return out
}

// withOrchestratorHost ensures the orchestrator's /32 is present in
// the prefix list before the cache renderer composes AllowedIPs. The
// registry's Snapshot already includes literal entries; this is the
// safety belt for "no entries yet" / "entries dropped the
// orchestrator" cases.
func withOrchestratorHost(prefixes []netip.Prefix, orchAddr netip.Addr) []netip.Prefix {
	hostBits := 32
	if orchAddr.Is6() && !orchAddr.Is4In6() {
		hostBits = 128
	}
	host := netip.PrefixFrom(orchAddr, hostBits)
	if slices.Contains(prefixes, host) {
		return prefixes
	}
	return append([]netip.Prefix{host}, prefixes...)
}

// nextHostInPrefix returns the address one above orchAddr within
// prefix (i.e. cache=.2 when orchestrator=.1). Used to derive the
// cache's WG address when rendering wg0.conf without requiring the
// operator to thread it through.
func nextHostInPrefix(prefix netip.Prefix, orchAddr netip.Addr) netip.Addr {
	next := orchAddr.Next()
	if prefix.Contains(next) {
		return next
	}
	// Fall back to the prefix's first usable host; tests cover the
	// common /30 baseline.
	addr := prefix.Masked().Addr()
	return addr.Next()
}

// parseHostAddr accepts either a bare IP or a "host/bits" string and
// returns the host IP. Config validation already enforces the
// "host/bits" form for transport.wg.local_addr, but accepting both
// keeps wg-quick-style operator habits ergonomic in tests.
func parseHostAddr(s string) (netip.Addr, error) {
	if strings.ContainsRune(s, '/') {
		pfx, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Addr{}, err
		}
		return pfx.Addr(), nil
	}
	return netip.ParseAddr(s)
}

// newRealLookup returns an acl.Lookup backed by net.DefaultResolver.
// Production callers get the orchestrator host's standard resolver
// chain (systemd-resolved, /etc/resolv.conf, etc.). The TTL field is
// not surfaced by the stdlib resolver — fall back to
// MinRefreshInterval per the resolver-loop contract.
func newRealLookup() acl.Lookup {
	return &realLookup{}
}

type realLookup struct{}

func (realLookup) LookupHost(ctx context.Context, host string) (acl.LookupResult, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return acl.LookupResult{}, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Unmap())
	}
	return acl.LookupResult{
		Addrs: out,
		// TTL omitted — stdlib doesn't surface it; the resolver loop
		// falls back to acl.MinRefreshInterval (30s).
		TTL: 0,
	}, nil
}
