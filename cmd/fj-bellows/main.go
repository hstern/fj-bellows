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
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"

	"github.com/hstern/fj-bellows/internal/bootstrap"
	"github.com/hstern/fj-bellows/internal/config"
	"github.com/hstern/fj-bellows/internal/forgejo"
	"github.com/hstern/fj-bellows/internal/orchestrator"
	"github.com/hstern/fj-bellows/internal/provider"

	// Register in-tree providers.
	dockerprov "github.com/hstern/fj-bellows/internal/provider/docker"
	_ "github.com/hstern/fj-bellows/internal/provider/linode"
)

func main() {
	configPath := flag.String("config", "/etc/fj-bellows/config.yaml", "path to config file")
	lockPath := flag.String("lock", "/run/fj-bellows.lock", "singleton lock file")
	runnerVersion := flag.String("runner-version", "12.10.1", "forgejo-runner version to install on workers")
	drain := flag.Bool("drain", true, "on shutdown, let in-flight jobs finish instead of interrupting them")
	drainTimeout := flag.Duration("drain-timeout", 0, "max time to wait for in-flight jobs when draining (0 = wait indefinitely)")
	destroyOnExit := flag.Bool("destroy-on-exit", false, "destroy all owned VMs on shutdown (for a permanent stop)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	opts := runOpts{
		configPath:    *configPath,
		lockPath:      *lockPath,
		runnerVersion: *runnerVersion,
		drain:         *drain,
		drainTimeout:  *drainTimeout,
		destroyOnExit: *destroyOnExit,
	}
	if err := run(opts, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type runOpts struct {
	configPath    string
	lockPath      string
	runnerVersion string
	drain         bool
	drainTimeout  time.Duration
	destroyOnExit bool
}

func run(opts runOpts, log *slog.Logger) error {
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return err
	}

	// config.yaml and the SSH key hold secrets; warn if other users can read
	// them. The Forgejo admin token rides in a header, so warn on plaintext URLs.
	warnLoosePerms(log, opts.configPath)
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
	if err := prov.Configure(cfg.ProviderConfig); err != nil {
		return err
	}

	fj := forgejo.New(cfg.Forgejo.URL, cfg.Forgejo.Scope, cfg.Forgejo.Token, cfg.Forgejo.Labels...)

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

	log.Info(
		"fj-bellows starting",
		"provider", cfg.Provider,
		"billing", prov.BillingModel().String(),
		"max_scale", cfg.Scale.Max,
		"poll", cfg.Poll.Interval.D().String(),
	)
	return orch.Run(ctx)
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
