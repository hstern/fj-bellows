// Command fj-bellows is a pluggable, ephemeral CI-runner autoscaler for
// Forgejo Actions. It polls the Actions job queue and provisions, warm-holds,
// and tears down cloud worker VMs per the provider's billing model.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
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
}

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
	var (
		signer  ssh.Signer
		authKey string
	)
	if cfg.Provider != config.ProviderDocker {
		signer, authKey, err = loadSSHKey(cfg.SSH.PrivateKeyFile)
		if err != nil {
			return err
		}
	}

	prov, err := provider.New(cfg.Provider)
	if err != nil {
		return err
	}
	// Hand the Linode provider the orchestrator's SSH public key so
	// the managed cache VM (if `cache:` is set) accepts ssh from the
	// operator for debugging. No tunnel; this is inbound-only debug
	// access. No-op for non-Linode providers.
	if l, ok := prov.(*linodeprov.Linode); ok && authKey != "" {
		// ssh.MarshalAuthorizedKey appends a trailing newline; Linode's
		// authorized_keys API rejects multi-line values with a 400.
		// The worker Provision path already does this trim on spec.AuthorizedKey.
		l.SetSSHAuthorizedKey(strings.TrimSpace(authKey))
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

	dispatcher, err := dispatcherFor(cfg, prov, signer)
	if err != nil {
		return err
	}

	orch := orchestrator.New(orchestrator.Config{
		Tag:           cfg.Tag,
		MaxScale:      cfg.Scale.Max,
		Labels:        cfg.Forgejo.Labels,
		PollInterval:  cfg.Poll.Interval.D(),
		RunnerVersion: opts.runnerVersion,
		ReadyFile:     bootstrap.DefaultReadyFile,
		AuthorizedKey: authKey,
		Teardown: orchestrator.TeardownPolicy{
			Model:       prov.BillingModel(),
			IdleTimeout: cfg.Poll.IdleTimeout.D(),
			HourMargin:  cfg.Poll.HourMargin.D(),
			BillingHour: cfg.Poll.BillingHour.D(),
		},
		DrainOnShutdown: opts.drain,
		DrainTimeout:    opts.drainTimeout,
		DestroyOnExit:   opts.destroyOnExit,
	}, prov, fj, dispatcher, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := startControlPlane(ctx, controlOpts{
		listen:       opts.controlListen,
		tokenFile:    opts.controlTokenFile,
		enableWrites: opts.enableControlWrites,
		configPath:   opts.configPath,
		cfg:          cfg,
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
// knob (the FJB-26 enableWrites flag) doesn't keep widening the signature.
type controlOpts struct {
	listen       string
	tokenFile    string
	enableWrites bool
	configPath   string
	cfg          *config.Config
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
		o:          orch,
		prov:       prov,
		logBus:     logBus,
		configPath: opts.configPath,
		cfg:        opts.cfg,
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
	o          *orchestrator.Orchestrator
	prov       provider.Provider
	logBus     *logbus.Bus
	configPath string

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
	}
}

func (b *controlBackend) PoolSnapshot() []control.WorkerView {
	in := b.o.PoolSnapshot()
	out := make([]control.WorkerView, 0, len(in))
	for _, w := range in {
		out = append(out, control.WorkerView{
			InstanceID: w.InstanceID,
			State:      w.State,
			IP:         w.IP,
			CreatedAt:  w.CreatedAt,
			LastBusy:   w.LastBusy,
			CurrentJob: w.CurrentJob,
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

// dispatcherFor selects and constructs the dispatcher matching cfg.Provider.
// The docker provider needs no SSH; everything else uses SSHDispatcher.
func dispatcherFor(cfg *config.Config, prov provider.Provider, signer ssh.Signer) (orchestrator.Dispatcher, error) {
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
