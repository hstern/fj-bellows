// Command fj-bellows is a pluggable, ephemeral CI-runner autoscaler for
// Forgejo Actions. It polls the Actions job queue and provisions, warm-holds,
// and tears down cloud worker VMs per the provider's billing model.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/bootstrap"
	"github.com/hstern/fj-bellows/internal/config"
	"github.com/hstern/fj-bellows/internal/control"
	"github.com/hstern/fj-bellows/internal/control/events"
	"github.com/hstern/fj-bellows/internal/control/logbus"
	"github.com/hstern/fj-bellows/internal/forgejo"
	"github.com/hstern/fj-bellows/internal/orchestrator"
	"github.com/hstern/fj-bellows/internal/provider"
	"github.com/hstern/fj-bellows/internal/transport/wg"
	"github.com/hstern/fj-bellows/internal/transport/wgboot"

	// Register in-tree providers.
	dockerprov "github.com/hstern/fj-bellows/internal/provider/docker"
	linodeprov "github.com/hstern/fj-bellows/internal/provider/linode"
)

func main() {
	configPath := flag.String("config", "/etc/fj-bellows/config.yaml", "path to config file")
	lockPath := flag.String("lock", "/run/fj-bellows.lock", "singleton lock file")
	runnerVersion := flag.String("runner-version", "12.10.1", "forgejo-runner version to install on workers")
	drain := flag.Bool("drain", true, "on shutdown, let in-flight jobs finish instead of interrupting them")
	drainTimeout := flag.Duration("drain-timeout", 0, "max time to wait for in-flight jobs when draining (0 = wait indefinitely)")
	destroyOnExit := flag.Bool("destroy-on-exit", false, "destroy all owned VMs on shutdown (for a permanent stop)")
	controlListen := flag.String("control-listen", "127.0.0.1:9876", "control plane listen address (TCP); empty disables")
	controlTokenFile := flag.String("control-token-file", "", "bearer-token file for the control plane (required for non-loopback binds; mode 0600)")
	enableControlWrites := flag.Bool("enable-control-writes", false, "expose mutating control RPCs (ForceReap, ForceProvision); off by default")
	// fjbagent (FJB-94). The agent's version implicitly tracks this
	// orchestrator's build (main.version), so there is no per-deployment
	// version choice — only a token file (required) and an optional URL
	// template override (default points at the matching github release).
	fjbAgentTokenFile := flag.String("fjbagent-token-file", "", "bearer-token file for fjbagent on workers (mode 0600); empty disables agent install")
	fjbAgentURLTmpl := flag.String("fjbagent-url", "", "URL template for the fjbagent binary; supports {{.Version}} (resolved from this build) and $rarch (resolved by cloud-init). Empty uses the default github releases URL.")
	flag.Parse()

	// Wrap the stderr text handler with a logbus tee so the control plane's
	// StreamLogs RPC can fan structured records out to subscribers without
	// disturbing stderr output. The bus also keeps a ring buffer of the
	// most recent records so a new operator can replay history.
	logBus := logbus.New()
	textHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(logbus.NewHandler(textHandler, logBus))

	opts := runOpts{
		configPath:          *configPath,
		lockPath:            *lockPath,
		runnerVersion:       *runnerVersion,
		drain:               *drain,
		drainTimeout:        *drainTimeout,
		destroyOnExit:       *destroyOnExit,
		controlListen:       *controlListen,
		controlTokenFile:    *controlTokenFile,
		enableControlWrites: *enableControlWrites,
		fjbAgentTokenFile:   *fjbAgentTokenFile,
		fjbAgentURLTmpl:     *fjbAgentURLTmpl,
	}
	if err := run(opts, log, logBus); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type runOpts struct {
	configPath          string
	lockPath            string
	runnerVersion       string
	drain               bool
	drainTimeout        time.Duration
	destroyOnExit       bool
	controlListen       string
	controlTokenFile    string
	enableControlWrites bool
	fjbAgentTokenFile   string
	fjbAgentURLTmpl     string
}

// version is the fj-bellows daemon's build version, stamped at link
// time via -ldflags "-X main.version=<git describe output>". It also
// drives the version of fjbagent installed on workers via cloud-init,
// since agent and orchestrator share a proto and must be built from
// the same commit.
var version = "dev"

func run(opts runOpts, log *slog.Logger, logBus *logbus.Bus) error {
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return err
	}

	warnStartupHygiene(log, cfg, opts.configPath)

	// Singleton lock: only one daemon may make provisioning decisions.
	release, err := acquireLock(opts.lockPath)
	if err != nil {
		return fmt.Errorf("acquire singleton lock %s: %w", opts.lockPath, err)
	}
	defer release()

	// SSH key is required only for providers that dispatch over SSH. A docker
	// deployment passes nothing into cloud-init and execs into containers.
	signer, authKey, err := loadSSHKeyForProvider(cfg)
	if err != nil {
		return err
	}

	prov, err := provider.New(cfg.Provider)
	if err != nil {
		return err
	}
	applyAuthorizedKeyToLinodeProvider(prov, authKey)
	// Propagate transport mode into the Linode provider so its managed
	// firewall synthesizes the right ACCEPT rules (tcp/22 for legacy SSH,
	// IPsec ports for cache-gateway). Duck-typed so providers that don't
	// implement the method (e.g. the docker provider, which has no
	// Linode-style firewall) are unaffected.
	applyTransportToProvider(prov, cfg.Transport)
	// Under cache-gateway mode, load (or generate) the orchestrator's WG
	// keypair BEFORE Configure runs. Configure provisions the cache VM,
	// and its cloud-init needs the orchestrator's public key baked in
	// so wg-quick on the cache can come up referencing the right peer
	// (FJB-99 Phase A). Phase B will move the wgboot.Boot call to AFTER
	// Configure and have it reuse this same key.
	if err := plumbOrchestratorWGPubkey(prov, cfg.Transport); err != nil {
		return err
	}
	// Bound the Configure-time network calls (provider sentinel fetches,
	// firewall API, etc.) so a hung upstream can't wedge startup forever.
	cfgCtx, cancelCfg := context.WithTimeout(context.Background(), 60*time.Second)
	if err := prov.Configure(cfgCtx, cfg.Tag, cfg.ProviderConfig); err != nil {
		cancelCfg()
		return err
	}
	cancelCfg()

	// Forgejo's job-queue ?labels= filter matches the bare label a workflow
	// declares in `runs_on`, so strip any `:scheme://image` binding before
	// passing labels to the client. Registration and the worker's --label arg
	// still see the full strings via the orchestrator config below. See #39.
	fj := forgejo.New(cfg.Forgejo.URL, cfg.Forgejo.Scope, cfg.Forgejo.Token, forgejo.BareLabels(cfg.Forgejo.Labels)...)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Boot the WireGuard cache-gateway transport stack (FJB-90) BEFORE
	// constructing the dispatcher so the cache-gateway dispatcher can
	// dial worker VPC IPs through the tunnel's netstack (FJB-92). The
	// stack owns the embedded WG tunnel, ACL registry + DNS resolver
	// loop, DNS responder, TCP proxy, UDP forwarder, and ICMP bridge.
	// Returns a closer that's safe to defer-call even when no stack
	// was started (legacy ssh mode / docker provider); wgStack is nil
	// in those cases.
	wgStack, wgClose, err := bootWGStack(ctx, cfg, prov, log)
	if err != nil {
		return err
	}
	defer wgClose()

	dispatcher, err := dispatcherFor(cfg, prov, signer, wgStack)
	if err != nil {
		return err
	}

	orchCfg, err := buildOrchestratorConfig(cfg, opts, version, authKey, prov)
	if err != nil {
		return err
	}
	orch := orchestrator.New(orchCfg, prov, fj, dispatcher, log)

	if err := startControlPlane(ctx, controlOpts{
		listen:       opts.controlListen,
		tokenFile:    opts.controlTokenFile,
		enableWrites: opts.enableControlWrites,
		configPath:   opts.configPath,
		cfg:          cfg,
		providerName: cfg.Provider,
	}, orch, prov, logBus, log); err != nil {
		return err
	}

	log.Info(
		"fj-bellows starting",
		"provider", cfg.Provider,
		"billing", prov.BillingModel().String(),
		"max_scale", cfg.Scale.Max,
		"poll", cfg.Poll.Interval.D().String(),
	)
	return orch.Run(ctx)
}

// controlOpts groups the wiring inputs for startControlPlane so adding a new
// knob doesn't keep widening the signature.
type controlOpts struct {
	listen       string
	tokenFile    string
	enableWrites bool
	configPath   string
	cfg          *config.Config
	providerName string
}

// startControlPlane spins up the operator-facing HTTP/RPC server on a side
// goroutine. Empty listen disables it (e.g. for tests or restricted deploys).
// If a token file is supplied, every Connect RPC must carry an
// Authorization: Bearer header matching its contents; /healthz and /metrics
// stay open. Returns an error only on bad operator config (missing token
// file, unreadable token, a non-loopback bind with no token, or
// -enable-control-writes set on a non-loopback bind with no token) — once
// it successfully arms the goroutine, runtime listen errors are logged.
func startControlPlane(ctx context.Context, opts controlOpts, orch *orchestrator.Orchestrator, prov provider.Provider, logBus *logbus.Bus, log *slog.Logger) error {
	if opts.listen == "" {
		return nil
	}
	var token string
	if opts.tokenFile != "" {
		t, err := control.LoadToken(opts.tokenFile)
		if err != nil {
			return fmt.Errorf("control token: %w", err)
		}
		token = t
	}
	loopback := control.IsLoopbackBind(opts.listen)
	if !loopback && token == "" {
		return fmt.Errorf("control-listen %q is not loopback but -control-token-file is unset; "+
			"either bind 127.0.0.1 or provide a token file", opts.listen)
	}
	// Mutating verbs on a non-loopback bind without a token are an outright
	// refusal: the bearer-token gate is the deployment's auth boundary, and
	// exposing force-* unauthenticated to the network is never the intent.
	// The loopback + writes + no-token combination is fine (the network is
	// the boundary).
	if opts.enableWrites && !loopback && token == "" {
		return fmt.Errorf("-enable-control-writes on non-loopback bind %q requires -control-token-file", opts.listen)
	}
	backend := &controlBackend{
		o:            orch,
		prov:         prov,
		providerName: opts.providerName,
		logBus:       logBus,
		configPath:   opts.configPath,
		cfg:          opts.cfg,
	}
	srv := control.NewServer(opts.listen, backend, log,
		control.WithBearerToken(token),
		control.WithControlWrites(opts.enableWrites))
	go func() {
		if err := srv.Run(ctx); err != nil {
			log.Error("control plane", "err", err)
		}
	}()
	return nil
}

// controlBackend adapts *orchestrator.Orchestrator (and the live provider,
// for cache-aware reports) to control.Backend so the orchestrator package
// stays free of generated-protobuf coupling.
//
// configMu protects cfg, which is swapped on a successful ReloadConfig.
// GetConfig and ReloadConfig both take it; no other adapter method touches
// the on-disk config struct (the orchestrator owns its own copy).
type controlBackend struct {
	o            *orchestrator.Orchestrator
	prov         provider.Provider
	providerName string
	logBus       *logbus.Bus
	configPath   string

	configMu sync.RWMutex
	cfg      *config.Config
}

func (b *controlBackend) Health(ctx context.Context) control.HealthStatus {
	s := b.o.Health(ctx)
	return control.HealthStatus{
		Healthy:            s.Healthy,
		LastTickAt:         s.LastTickAt,
		LastProviderListAt: s.LastProviderListAt,
		LastForgejoPollAt:  s.LastForgejoPollAt,
		Paused:             s.Paused,
	}
}

func (b *controlBackend) PoolSnapshot() []control.WorkerView {
	in := b.o.PoolSnapshot()
	out := make([]control.WorkerView, 0, len(in))
	for _, w := range in {
		out = append(out, control.WorkerView{
			InstanceID:     w.InstanceID,
			State:          w.State,
			IP:             w.IP,
			VPCIP:          w.VPCIP,
			CreatedAt:      w.CreatedAt,
			LastBusy:       w.LastBusy,
			CurrentJob:     w.CurrentJob,
			PaidHourEndAt:  w.PaidHourEndAt,
			ReapEligibleAt: w.ReapEligibleAt,
			BillingModel:   w.BillingModel,
		})
	}
	return out
}

func (b *controlBackend) Kick(ctx context.Context) (control.ReconcileResult, error) {
	r, err := b.o.Kick(ctx)
	if err != nil {
		return control.ReconcileResult{}, err
	}
	return control.ReconcileResult{
		Provisioned: r.Provisioned,
		Dispatched:  r.Dispatched,
		Reaped:      r.Reaped,
		Adopted:     r.Adopted,
		Dropped:     r.Dropped,
		Errors:      r.Errors,
	}, nil
}

func (b *controlBackend) Subscribe() (<-chan events.Event, func()) {
	return b.o.Subscribe()
}

func (b *controlBackend) SubscribeLogs(filter logbus.Filter) (<-chan logbus.Record, func()) {
	return b.logBus.SubscribeFiltered(filter)
}

func (b *controlBackend) LogHistory(n int, filter logbus.Filter) []logbus.Record {
	return b.logBus.History(n, filter)
}

func (b *controlBackend) ForceReap(ctx context.Context, instanceID string) error {
	return b.o.ForceReap(ctx, instanceID)
}

func (b *controlBackend) ForceProvision(ctx context.Context) (string, error) {
	return b.o.ForceProvision(ctx)
}

func (b *controlBackend) Pause(ctx context.Context) {
	b.o.Pause(ctx)
}

func (b *controlBackend) Resume(ctx context.Context) {
	b.o.Resume(ctx)
}

// GetConfig serialises the daemon's live config (with secrets redacted) as
// YAML and returns the path the config was originally loaded from. The
// orchestrator owns the hot-reloadable subset internally; controlBackend's
// stored *config.Config is the on-disk source of truth, refreshed on every
// successful ReloadConfig.
func (b *controlBackend) GetConfig(_ context.Context) (string, string) {
	b.configMu.RLock()
	cfg := b.cfg
	b.configMu.RUnlock()
	if cfg == nil {
		return "", b.configPath
	}
	redacted := config.Redact(cfg)
	out, err := yaml.Marshal(redacted)
	if err != nil {
		// Marshal of a struct with primitive fields + a yaml.Node can't
		// realistically fail; surface a one-line marker rather than
		// panicking from a control RPC.
		return fmt.Sprintf("# marshal error: %v\n", err), b.configPath
	}
	return string(out), b.configPath
}

// ReloadConfig re-reads config.yaml from disk, validates it, and hands the
// hot-reloadable subset to the orchestrator. On success the backend's
// in-memory *config.Config is swapped so a subsequent GetConfig reflects the
// new values. On failure (read/parse error or non-hot field changed), the
// in-memory config and the orchestrator are left untouched.
func (b *controlBackend) ReloadConfig(_ context.Context) ([]string, error) {
	newCfg, err := config.Load(b.configPath)
	if err != nil {
		return nil, fmt.Errorf("reload %s: %w", b.configPath, err)
	}
	// Build the candidate orchestrator config by overlaying the on-disk
	// values onto whatever the orchestrator is running now. CLI-flag-only
	// fields (RunnerVersion, drain settings, ReadyFile, AuthorizedKey,
	// Tag, billing model) keep their startup values; ApplyHotConfig will
	// reject any drift on those.
	cur := b.o.CurrentConfig()
	next := cur
	next.MaxScale = newCfg.Scale.Max
	next.Labels = newCfg.Forgejo.Labels
	next.PollInterval = newCfg.Poll.Interval.D()
	next.Teardown.IdleTimeout = newCfg.Poll.IdleTimeout.D()
	next.Teardown.HourMargin = newCfg.Poll.HourMargin.D()
	next.Teardown.BillingHour = newCfg.Poll.BillingHour.D()
	// Non-hot fields the on-disk file carries: surfacing a change here
	// before ApplyHotConfig keeps the error message close to the file
	// the operator just edited.
	if newCfg.Tag != cur.Tag {
		return nil, fmt.Errorf("reload rejected: tag changed (was %q, now %q); restart required",
			cur.Tag, newCfg.Tag)
	}

	changed, err := b.o.ApplyHotConfig(next)
	if err != nil {
		return nil, err
	}
	b.configMu.Lock()
	b.cfg = newCfg
	b.configMu.Unlock()
	return changed, nil
}

// ExecOnWorker forwards to the orchestrator and unpacks ExecResult into
// the flat shape the control.Backend interface expects (so the control
// package stays free of orchestrator-side types).
func (b *controlBackend) ExecOnWorker(ctx context.Context, instanceID, command string) ([]byte, []byte, int32, int64, int64, error) {
	r, err := b.o.ExecOnWorker(ctx, instanceID, command)
	if err != nil {
		return nil, nil, 0, 0, 0, err
	}
	return r.Stdout, r.Stderr, r.ExitCode, r.TruncatedStdout, r.TruncatedStderr, nil
}

// ProviderInfo type-asserts the live provider to the optional
// InfoProvider surface and returns its key/value map, plus the
// configured provider slug. Providers that don't implement
// InfoProvider answer with an empty map; the slug is always
// populated so the operator can tell apart "provider doesn't expose
// anything" from "wrong provider name on the wire". Keeps the
// provider.Provider interface from growing every time we add a
// provider-debug surface (FJB-31).
func (b *controlBackend) ProviderInfo(ctx context.Context) (string, map[string]string) {
	if ip, ok := b.prov.(provider.InfoProvider); ok {
		return b.providerName, ip.Info(ctx)
	}
	return b.providerName, map[string]string{}
}

// CacheStatus walks the provider for cache info if it supports it (Linode
// does; docker doesn't). The type-assertion keeps the orchestrator package
// free of provider-specific imports.
func (b *controlBackend) CacheStatus(ctx context.Context) *control.CacheStatus {
	type cacheReporter interface {
		CacheStatus(ctx context.Context) *linodeprov.CacheStatus
	}
	cr, ok := b.prov.(cacheReporter)
	if !ok {
		return nil
	}
	s := cr.CacheStatus(ctx)
	if s == nil {
		return nil
	}
	return &control.CacheStatus{
		Present:         s.Present,
		AdoptedExisting: s.AdoptedExisting,
		LinodeID:        s.LinodeID,
		VPCIP:           s.VPCIP,
		BucketRegion:    s.BucketRegion,
		BucketLabel:     s.BucketLabel,
		VMState:         s.VMState,
	}
}

// bootWGStack starts the embedded WireGuard cache-gateway transport
// (FJB-90) when transport.mode = cache-gateway. Returns (nil, no-op
// closer, nil) on any other mode (legacy SSH, docker provider) —
// callers then run with the pre-FJB-90 dispatch path unchanged.
//
// Failures (key unreadable, ACL parse, DNS bind) bubble up as
// daemon-fatal: the operator chose cache-gateway mode and the stack
// can't come up; running with a partial transport would be worse
// than refusing to start.
//
// The returned *wgboot.Stack lets the dispatcher inject the tunnel's
// netstack DialContext (FJB-92) — workers' VPC IPs are unreachable
// from the host kernel's route table under the netstack transport.
//
// The function pulls the cache VPC IP and (when available) the
// worker VPC subnet from the live provider via duck-typed interfaces
// — keeps cmd/fj-bellows free of provider-internal types.
func bootWGStack(ctx context.Context, cfg *config.Config, prov provider.Provider, log *slog.Logger) (*wgboot.Stack, func(), error) {
	if cfg.Transport.Mode != config.TransportCacheGateway {
		return nil, func() {}, nil
	}
	cache := cacheRenderInputs(ctx, cfg, prov)
	// Only run the FJB-99 bootstrap poll when the operator hasn't
	// statically configured a peer. Skipping the 3-minute poll under
	// static config keeps daemon startup fast and matches the
	// back-compat path the FJB-91 e2e harness exercises against the
	// persistent test cache.
	var cachePubkey, cacheEndpoint string
	if cfg.Transport.WG != nil && (cfg.Transport.WG.Peer.PublicKey == "" || cfg.Transport.WG.Peer.Endpoint == "") {
		cachePubkey, cacheEndpoint = discoverCachePeer(ctx, prov, log)
	}
	stack, err := wgboot.Boot(ctx, wgboot.Config{
		Transport:     cfg.Transport,
		ForgejoURL:    cfg.Forgejo.URL,
		ACLSink:       aclSinkFor(prov),
		Cache:         cache,
		Logger:        log,
		CachePubkey:   cachePubkey,
		CacheEndpoint: cacheEndpoint,
	})
	if err != nil {
		return nil, func() {}, fmt.Errorf("transport bootstrap: %w", err)
	}
	return stack, func() {
		if cerr := stack.Close(); cerr != nil {
			log.Warn("wgboot: shutdown", "err", cerr)
		}
	}, nil
}

// discoverCachePeer runs the orchestrator side of the FJB-99 bootstrap
// loop: poll the managed cache's Object Storage bucket for the cache's
// WG pubkey (published by cache cloud-init's fjb-wg-bootstrap.sh) and
// read the cache's public IPv4 from the Linode API to build the WG
// peer endpoint. Returns empty strings (no error) when the provider
// can't supply these — wgboot then falls back to the static
// transport.wg.peer.{public_key,endpoint} config knobs and errors at
// boot if those are also empty.
//
// Bounded at 3 minutes — the cache cloud-init steady-state (package
// install + wireguard + pubkey upload) typically completes well under
// 2 minutes, so the bound is generous but not unbounded.
func discoverCachePeer(ctx context.Context, prov provider.Provider, log *slog.Logger) (pubkey, endpoint string) {
	type pubkeyWaiter interface {
		WaitForCacheWGPubkey(ctx context.Context, timeout time.Duration) (string, error)
	}
	type endpointReader interface {
		CachePublicEndpoint(ctx context.Context) (string, error)
	}
	if pw, ok := prov.(pubkeyWaiter); ok {
		start := time.Now()
		if pk, err := pw.WaitForCacheWGPubkey(ctx, 3*time.Minute); err == nil {
			log.Info("wgboot: cache wg-pubkey discovered", "elapsed", time.Since(start))
			pubkey = pk
		} else {
			log.Warn("wgboot: cache wg-pubkey discovery failed; falling back to static config", "err", err)
		}
	}
	if er, ok := prov.(endpointReader); ok {
		if ep, err := er.CachePublicEndpoint(ctx); err == nil {
			endpoint = ep
		} else {
			log.Warn("wgboot: cache public endpoint discovery failed; falling back to static config", "err", err)
		}
	}
	return pubkey, endpoint
}

// cacheRenderInputs pulls the cache-side facts the renderer needs out
// of whatever live provider state is available. Today only the Linode
// provider carries a managed cache; the function returns whatever it
// can find and lets the renderer's validate() reject missing fields.
//
// FJB-92 added the WorkerVPCSubnet lookup: the renderer needs it to
// emit the orchestrator → worker reverse-direction ACCEPT rule, and
// wgboot needs it to include the worker VPC CIDR in the orchestrator's
// peer AllowedIPs (otherwise the netstack won't encapsulate outbound
// dispatcher dials).
func cacheRenderInputs(ctx context.Context, cfg *config.Config, prov provider.Provider) wgboot.CacheRenderInputs {
	in := wgboot.CacheRenderInputs{}
	type cacheReporter interface {
		CacheStatus(ctx context.Context) *linodeprov.CacheStatus
	}
	if cr, ok := prov.(cacheReporter); ok {
		if s := cr.CacheStatus(ctx); s != nil {
			in.CacheVPCIP = s.VPCIP
		}
	}
	type workerVPCReporter interface {
		WorkerVPCSubnet() string
	}
	if wr, ok := prov.(workerVPCReporter); ok {
		in.WorkerVPCSubnet = wr.WorkerVPCSubnet()
	}
	_ = cfg // reserved for future config-driven render inputs (FJB-87 push path)
	return in
}

// aclSinkFor returns a sink func that hands the registry adapter to
// the provider's SetACLSource method via duck-typing. Providers that
// don't carry a managed cache return a no-op sink (sink = nil).
func aclSinkFor(prov provider.Provider) func(wgboot.ACLSource) {
	type setter interface {
		SetACLSource(linodeprov.ACLSnapshotSource)
	}
	s, ok := prov.(setter)
	if !ok {
		return nil
	}
	return func(src wgboot.ACLSource) {
		// linodeprov.ACLSnapshotSource and wgboot.ACLSource share the
		// AllowedIPsCIDRs() string-slice method shape; adapt via an
		// inline shim so we don't tie the two packages together.
		s.SetACLSource(aclSnapshotShim{src: src})
	}
}

// aclSnapshotShim bridges wgboot.ACLSource → linodeprov.ACLSnapshotSource.
type aclSnapshotShim struct {
	src wgboot.ACLSource
}

func (a aclSnapshotShim) AllowedIPsCIDRs() []string {
	return a.src.AllowedIPsCIDRs()
}

// applyTransportToProvider propagates the top-level transport config
// into the active provider via duck-typed interfaces, so providers
// that don't care (e.g. docker) are unaffected.
//
//   - SetTransportMode(string): drives the firewall rule synthesis
//     (FJB-65) and worker cache-extras template selection (FJB-74).
//   - SetTunnelRoutes([]string): supplies the LAN-side CIDRs the
//     worker cloud-init renders as `ip route` commands under
//     cache-gateway mode (FJB-74). No-op when no tunnel block.
//   - SetWGListenPort(int): supplies the cache nanode's WireGuard
//     listen port so the firewall rule synthesis covers it (FJB-89).
//     No-op when no wg block.
func applyTransportToProvider(prov provider.Provider, t config.Transport) {
	if tp, ok := prov.(interface{ SetTransportMode(string) }); ok {
		tp.SetTransportMode(t.Mode)
	}
	if tr, ok := prov.(interface{ SetTunnelRoutes([]string) }); ok && t.Tunnel != nil {
		tr.SetTunnelRoutes(t.Tunnel.Routes)
	}
	if wp, ok := prov.(interface{ SetWGListenPort(int) }); ok && t.WG != nil {
		wp.SetWGListenPort(t.WG.ListenPort)
	}
}

// plumbOrchestratorWGPubkey loads (or generates) the orchestrator's WG
// keypair under cache-gateway mode and pushes the public key into the
// provider so the cache cloud-init can bake it in as the WG peer
// pubkey (FJB-99 Phase A). Duck-typed for the same reason as
// applyTransportToProvider; providers that don't carry a managed cache
// skip the push.
//
// No-op for ssh-mode deployments (no WG block configured) and for
// providers without SetOrchestratorWGPubkey (e.g. docker).
func plumbOrchestratorWGPubkey(prov provider.Provider, t config.Transport) error {
	if t.Mode != config.TransportCacheGateway || t.WG == nil {
		return nil
	}
	priv, err := wg.LoadOrGenerateKey(t.WG.PrivateKeyFile)
	if err != nil {
		return fmt.Errorf("orchestrator wg key: %w", err)
	}
	if sp, ok := prov.(interface{ SetOrchestratorWGPubkey(string) }); ok {
		sp.SetOrchestratorWGPubkey(priv.PublicKey().String())
	}
	return nil
}

// sshDispatcherFrom builds the SSH dispatcher from config.
func sshDispatcherFrom(cfg *config.Config, signer ssh.Signer) *orchestrator.SSHDispatcher {
	return &orchestrator.SSHDispatcher{
		User:        cfg.SSH.User,
		Port:        cfg.SSH.Port,
		Signer:      signer,
		ForgejoURL:  cfg.Forgejo.URL,
		Labels:      cfg.Forgejo.Labels,
		ReadyFile:   bootstrap.DefaultReadyFile,
		ReadyWait:   5 * time.Minute,
		DialTimeout: 15 * time.Second,
	}
}

// cacheGatewayDispatcherFrom builds the cache-gateway dispatcher
// (FJB-64). Distinct type from SSHDispatcher: deliberately does NOT
// implement HostKeyPinner so the orchestrator's host-key seeding
// logic auto-skips, and the dispatch session carries no reverse
// port-forward or /etc/hosts mutation (workers reach LAN destinations
// via the cache nanode's DNS resolver + IPsec routing).
//
// dialFn — when non-nil — is the netstack DialContext from the WG
// tunnel (FJB-92): worker VPC IPs are only reachable through the
// embedded netstack, so the dispatcher dials via the tunnel instead
// of the host kernel's route table. Nil falls back to the dispatcher's
// internal net.Dialer (only useful in unit tests with loopback fakes).
func cacheGatewayDispatcherFrom(cfg *config.Config, signer ssh.Signer, dialFn func(context.Context, string, string) (net.Conn, error)) *orchestrator.CacheGatewayDispatcher {
	return &orchestrator.CacheGatewayDispatcher{
		User:        cfg.SSH.User,
		Port:        cfg.SSH.Port,
		Signer:      signer,
		ForgejoURL:  cfg.Forgejo.URL,
		Labels:      cfg.Forgejo.Labels,
		ReadyFile:   bootstrap.DefaultReadyFile,
		ReadyWait:   5 * time.Minute,
		DialTimeout: 15 * time.Second,
		DialFn:      dialFn,
	}
}

// dispatcherFor selects and constructs the dispatcher matching the
// active provider + transport mode. Selection order:
//
//  1. Docker provider: docker-exec dispatcher (no SSH).
//  2. transport.mode = cache-gateway: CacheGatewayDispatcher (SSH
//     dial via worker VPC IP through the netstack tunnel; FJB-54,
//     FJB-92).
//  3. Default: SSHDispatcher (legacy SSH-on-public-IP).
//
// wgStack is non-nil iff transport.mode = cache-gateway: it supplies
// the netstack DialContext the cache-gateway dispatcher must use to
// reach worker VPC IPs (FJB-92).
func dispatcherFor(cfg *config.Config, prov provider.Provider, signer ssh.Signer, wgStack *wgboot.Stack) (orchestrator.Dispatcher, error) {
	if cfg.Provider == config.ProviderDocker {
		dp, ok := prov.(*dockerprov.Docker)
		if !ok {
			return nil, fmt.Errorf("provider %q registered under unexpected type %T", cfg.Provider, prov)
		}
		runner := dockerprov.NewDefaultRunner(dp.DockerBin())
		return dockerprov.NewExecDispatcher(
			runner,
			dp.DockerBin(),
			cfg.Forgejo.URL,
			cfg.Forgejo.Labels,
			dp.WaitTimeout(),
		), nil
	}
	if cfg.Transport.Mode == config.TransportCacheGateway {
		var dialFn func(context.Context, string, string) (net.Conn, error)
		if wgStack != nil && wgStack.Tunnel != nil {
			dialFn = wgStack.Tunnel.DialContext
		}
		return cacheGatewayDispatcherFrom(cfg, signer, dialFn), nil
	}
	return sshDispatcherFrom(cfg, signer), nil
}

// loadSSHKey reads a PEM private key file and returns the signer plus its
// authorized_keys public-key line to inject at provision time.
func loadSSHKey(path string) (ssh.Signer, string, error) {
	//nolint:gosec // G304: path is the operator-supplied SSH key file, not user input.
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read ssh key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, "", fmt.Errorf("parse ssh key: %w", err)
	}
	authLine := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	return signer, authLine, nil
}

// warnStartupHygiene logs warnings for common operator mistakes the daemon
// can still run through: world-readable secret files, plaintext Forgejo URL,
// and the default instance tag (which is unique-per-deployment-safe but
// silently destroys peer deployments on the same cloud account).
func warnStartupHygiene(log *slog.Logger, cfg *config.Config, configPath string) {
	warnLoosePerms(log, configPath)
	if cfg.SSH.PrivateKeyFile != "" {
		warnLoosePerms(log, cfg.SSH.PrivateKeyFile)
	}
	if !strings.HasPrefix(strings.ToLower(cfg.Forgejo.URL), "https://") {
		log.Warn("forgejo.url is not https; the admin token will be sent in plaintext", "url", cfg.Forgejo.URL)
	}
	if cfg.Tag == config.DefaultTag {
		log.Warn("using the default instance tag; set a unique 'tag' per deployment, "+
			"or multiple fj-bellows deployments on the same cloud account will adopt and destroy each other's VMs",
			"tag", cfg.Tag)
	}
}

// warnLoosePerms logs a warning if a secret file is readable by group or other.
func warnLoosePerms(log *slog.Logger, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		log.Warn("secret file is readable by other users; restrict to 0600",
			"path", path, "mode", fmt.Sprintf("%04o", mode))
	}
}

// acquireLock takes an exclusive advisory lock on path, returning a release
// func. It fails fast if another daemon already holds it.
func acquireLock(path string) (func(), error) {
	//nolint:gosec // G304: path is the operator-supplied lock file, not user input.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another instance is running: %w", err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// loadFJBAgentSettings reads the fjbagent token from disk (if configured)
// and resolves the binary download URL against the daemon's own build
// version. Returns the empty-string pair when -fjbagent-token-file is
// unset (agent install disabled).
func loadFJBAgentSettings(opts runOpts, buildVersion string) (token, url string, err error) {
	if opts.fjbAgentTokenFile == "" {
		// Empty token file = agent install disabled; nothing to load.
		return "", "", nil
	}
	tok, err := os.ReadFile(opts.fjbAgentTokenFile)
	if err != nil {
		return "", "", fmt.Errorf("read fjbagent token: %w", err)
	}
	t := strings.TrimSpace(string(tok))
	if t == "" {
		return "", "", fmt.Errorf("fjbagent token file %s is empty", opts.fjbAgentTokenFile)
	}
	resolvedURL, err := bootstrap.ResolveAgentDownloadURL(opts.fjbAgentURLTmpl, buildVersion)
	if err != nil {
		return "", "", fmt.Errorf("resolve fjbagent URL: %w", err)
	}
	return t, resolvedURL, nil
}

// buildOrchestratorConfig assembles the orchestrator.Config from the
// loaded config + flags. Extracted from run() so the latter stays under
// the gocyclo budget after Phase B added the FJB-94 token-load branch.
func buildOrchestratorConfig(cfg *config.Config, opts runOpts, buildVersion, authKey string, prov provider.Provider) (orchestrator.Config, error) {
	fjbAgentToken, fjbAgentURL, err := loadFJBAgentSettings(opts, buildVersion)
	if err != nil {
		return orchestrator.Config{}, err
	}
	_ = prov // reserved for Phase C: provider-aware SetFJBAgent wiring lives in its own helper there.
	return orchestrator.Config{
		Tag:                 cfg.Tag,
		MaxScale:            cfg.Scale.Max,
		Labels:              cfg.Forgejo.Labels,
		PollInterval:        cfg.Poll.Interval.D(),
		RunnerVersion:       opts.runnerVersion,
		ReadyFile:           bootstrap.DefaultReadyFile,
		AuthorizedKey:       authKey,
		TransportMode:       cfg.Transport.Mode,
		FJBAgentDownloadURL: fjbAgentURL,
		FJBAgentToken:       fjbAgentToken,
		Teardown: orchestrator.TeardownPolicy{
			Model:       prov.BillingModel(),
			IdleTimeout: cfg.Poll.IdleTimeout.D(),
			HourMargin:  cfg.Poll.HourMargin.D(),
			BillingHour: cfg.Poll.BillingHour.D(),
		},
		DrainOnShutdown: opts.drain,
		DrainTimeout:    opts.drainTimeout,
		DestroyOnExit:   opts.destroyOnExit,
	}, nil
}

// applyAuthorizedKeyToLinodeProvider hands the operator's SSH authorized-key
// line to the Linode provider so the managed cache VM accepts ssh debug
// access. No-op for non-Linode providers, no-op when authKey is empty
// (docker-only deployments). Extracted to keep run()'s cyclomatic
// complexity under the gocyclo budget after the FJB-94 Phase B token-load
// branch landed.
func applyAuthorizedKeyToLinodeProvider(prov provider.Provider, authKey string) {
	if authKey == "" {
		return
	}
	l, ok := prov.(*linodeprov.Linode)
	if !ok {
		return
	}
	// ssh.MarshalAuthorizedKey appends a trailing newline; Linode's
	// authorized_keys API rejects multi-line values with a 400. The
	// worker Provision path already does this trim on spec.AuthorizedKey.
	l.SetSSHAuthorizedKey(strings.TrimSpace(authKey))
}

// loadSSHKeyForProvider loads the SSH key only for providers that dispatch
// over SSH; docker dispatches via container exec and needs nothing. Empty
// returns are safe for both signer and authKey on the docker path. Extracted
// to keep run() under the gocyclo budget after FJB-94 Phase B.
func loadSSHKeyForProvider(cfg *config.Config) (ssh.Signer, string, error) {
	if cfg.Provider == config.ProviderDocker {
		return nil, "", nil
	}
	return loadSSHKey(cfg.SSH.PrivateKeyFile)
}
