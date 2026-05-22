// Package orchestrator is the always-on daemon: it polls the Forgejo job
// queue, reconciles waiting jobs against Forgejo runners and provider
// instances, provisions/keeps-warm/tears-down worker VMs per the billing
// model, and sweeps orphans. The reconcile loop is the single writer of
// provisioning decisions.
package orchestrator

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hstern/fj-bellows/internal/bootstrap"
	"github.com/hstern/fj-bellows/internal/forgejo"
	"github.com/hstern/fj-bellows/internal/provider"
)

// JobSource is the slice of the Forgejo API the orchestrator consumes.
// *forgejo.Client satisfies it; tests supply a mock.
type JobSource interface {
	WaitingJobs(ctx context.Context) ([]forgejo.WaitingJob, error)
	RegisterEphemeral(ctx context.Context, name string, labels []string) (forgejo.Registration, error)
	ListRunners(ctx context.Context) ([]forgejo.Runner, error)
	DeleteRunner(ctx context.Context, id int64) error
}

// Config holds the orchestrator's runtime parameters, decoupled from the
// on-disk config struct.
type Config struct {
	Tag           string
	MaxScale      int
	Labels        []string
	PollInterval  time.Duration
	RunnerVersion string
	ReadyFile     string
	Teardown      TeardownPolicy
	AuthorizedKey string

	// DrainOnShutdown lets in-flight jobs finish on shutdown instead of being
	// interrupted immediately.
	DrainOnShutdown bool
	// DrainTimeout bounds how long to wait for in-flight jobs when draining;
	// 0 waits indefinitely (rely on the supervisor's stop timeout).
	DrainTimeout time.Duration
	// DestroyOnExit tears down all owned VMs on shutdown. Default false leaves
	// warm VMs for a restarted daemon to readopt; set true for a permanent stop.
	DestroyOnExit bool
}

// Orchestrator wires the pool, provider, job source, and dispatcher together.
type Orchestrator struct {
	cfg  Config
	prov provider.Provider
	jobs JobSource
	disp Dispatcher
	pool *Pool
	log  *slog.Logger

	wg sync.WaitGroup // tracks in-flight dispatch/provision/teardown goroutines

	mu          sync.Mutex
	pending     int                 // in-flight provisions not yet in the pool
	dispatching map[string]struct{} // job handles currently being served
	active      map[string]struct{} // runner UUIDs we registered and still expect
	now         func() time.Time    // injectable clock for tests

	// reapSeen tracks runner UUIDs that looked like zombies last tick; only
	// reaped after two consecutive sightings so a just-registered runner is not
	// deleted in the window before its UUID is recorded. Touched only by the
	// single reconcile goroutine, so it needs no lock.
	reapSeen map[string]struct{}
}

// New builds an orchestrator.
func New(cfg Config, prov provider.Provider, jobs JobSource, disp Dispatcher, log *slog.Logger) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	if cfg.ReadyFile == "" {
		cfg.ReadyFile = bootstrap.DefaultReadyFile
	}
	return &Orchestrator{
		cfg:         cfg,
		prov:        prov,
		jobs:        jobs,
		disp:        disp,
		pool:        NewPool(),
		log:         log,
		dispatching: map[string]struct{}{},
		active:      map[string]struct{}{},
		reapSeen:    map[string]struct{}{},
		now:         time.Now,
	}
}

// Run reconciles on each tick until ctx (the shutdown signal) is cancelled,
// then drains in-flight work. Jobs run under an independent context so a
// shutdown can choose to let them finish (drain) rather than interrupt them.
func (o *Orchestrator) Run(ctx context.Context) error {
	jobCtx, cancelJobs := context.WithCancel(context.Background())
	defer cancelJobs()

	t := time.NewTicker(o.cfg.PollInterval)
	defer t.Stop()
	o.Reconcile(jobCtx)
	for {
		select {
		case <-ctx.Done():
			o.shutdown(cancelJobs)
			return nil
		case <-t.C:
			o.Reconcile(jobCtx)
		}
	}
}

// shutdown stops scheduling new work and waits for in-flight goroutines. With
// DrainOnShutdown it lets running jobs finish (bounded by DrainTimeout, 0 =
// indefinitely); otherwise it interrupts them immediately. Optionally destroys
// owned VMs on exit.
func (o *Orchestrator) shutdown(cancelJobs context.CancelFunc) {
	if !o.cfg.DrainOnShutdown {
		o.log.Info("shutting down; interrupting in-flight jobs")
		cancelJobs()
	} else {
		o.log.Info("shutting down; draining in-flight jobs", "timeout", o.cfg.DrainTimeout.String())
	}

	done := make(chan struct{})
	go func() { o.wg.Wait(); close(done) }()

	if o.cfg.DrainOnShutdown && o.cfg.DrainTimeout > 0 {
		select {
		case <-done:
		case <-time.After(o.cfg.DrainTimeout):
			o.log.Warn("drain timeout reached; interrupting remaining jobs")
			cancelJobs()
			<-done
		}
	} else {
		<-done
	}

	if o.cfg.DestroyOnExit {
		o.destroyAll()
	}
}

// destroyAll tears down every instance currently in the pool, using a fresh
// bounded context since the job context is already cancelled by shutdown.
func (o *Orchestrator) destroyAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, n := range o.pool.Snapshot() {
		if err := o.prov.Destroy(ctx, n.InstanceID); err != nil {
			o.log.Error("destroy on exit", "id", n.InstanceID, "err", err)
			continue
		}
		o.pool.Delete(n.InstanceID)
		o.log.Info("destroyed on exit", "id", n.InstanceID)
	}
}

// Reconcile performs one convergence pass: sync the pool to provider truth,
// dispatch waiting jobs, provision capacity, and apply teardown.
func (o *Orchestrator) Reconcile(ctx context.Context) {
	insts, err := o.prov.List(ctx, o.cfg.Tag)
	if err != nil {
		o.log.Error("list instances", "err", err)
		return
	}
	o.syncPool(insts)

	jobs, err := o.jobs.WaitingJobs(ctx)
	if err != nil {
		o.log.Error("poll waiting jobs", "err", err)
		jobs = nil
	}
	jobs = filterServiceable(jobs, o.cfg.Labels)

	o.dispatchJobs(ctx, jobs)
	o.applyTeardown(ctx)
	o.reapZombieRunners(ctx)
}

// reapZombieRunners deletes runner registrations that are ours (name carries
// the tag prefix) but that we are no longer running a job for — e.g. a VM that
// died after registering but before one-job completed, leaving a dangling
// registration Forgejo never auto-removed. A runner must look orphaned for two
// consecutive ticks before deletion, closing the race against a runner whose
// UUID we have not recorded as active yet.
func (o *Orchestrator) reapZombieRunners(ctx context.Context) {
	runners, err := o.jobs.ListRunners(ctx)
	if err != nil {
		o.log.Error("list runners", "err", err)
		return
	}
	prefix := o.cfg.Tag + "-"
	seen := map[string]struct{}{}
	for _, r := range runners {
		if !strings.HasPrefix(r.Name, prefix) || o.isActive(r.UUID) {
			continue
		}
		if _, twice := o.reapSeen[r.UUID]; !twice {
			seen[r.UUID] = struct{}{} // first sighting; revisit next tick
			continue
		}
		if err := o.jobs.DeleteRunner(ctx, r.ID); err != nil {
			o.log.Error("reap zombie runner", "uuid", r.UUID, "name", r.Name, "err", err)
			seen[r.UUID] = struct{}{} // keep trying next tick
			continue
		}
		o.log.Info("reaped zombie runner", "uuid", r.UUID, "name", r.Name)
	}
	o.reapSeen = seen
}

// syncPool adopts provider instances unknown to the pool (crash recovery) and
// drops pool nodes the provider no longer reports. Provisioning nodes are never
// dropped: a freshly created VM may not appear in List yet.
func (o *Orchestrator) syncPool(insts []provider.Instance) {
	now := o.now()
	seen := map[string]struct{}{}
	for _, in := range insts {
		seen[in.ID] = struct{}{}
		if _, ok := o.pool.Get(in.ID); !ok {
			o.pool.Put(&Node{
				InstanceID: in.ID,
				State:      StateIdle, // adopt as warm; readiness re-confirmed on dispatch
				IP:         in.IPv4,
				CreatedAt:  in.CreatedAt,
				LastBusy:   now,
			})
			o.log.Info("adopted orphan instance", "id", in.ID, "ip", in.IPv4)
		}
	}
	for _, n := range o.pool.Snapshot() {
		if _, ok := seen[n.InstanceID]; ok {
			continue
		}
		if n.State == StateProvisioning {
			continue
		}
		o.pool.Delete(n.InstanceID)
		o.log.Info("dropped vanished instance", "id", n.InstanceID, "state", n.State)
	}
}

// dispatchJobs assigns waiting jobs to idle nodes and provisions capacity for
// the rest, bounded by MaxScale.
func (o *Orchestrator) dispatchJobs(ctx context.Context, jobs []forgejo.WaitingJob) {
	idle := o.pool.ByState(StateIdle)
	next := 0
	needProvision := 0
	for _, job := range jobs {
		if o.isDispatching(job.Handle) {
			continue
		}
		if next < len(idle) {
			o.dispatch(ctx, idle[next], job)
			next++
			continue
		}
		needProvision++
	}
	if needProvision == 0 {
		return
	}
	active := o.pool.Len() + o.pendingCount()
	canAdd := o.cfg.MaxScale - active
	for i := 0; i < needProvision && i < canAdd; i++ {
		o.provisionOne(ctx)
	}
}

// dispatch marks a node Busy and serves the job in a goroutine.
func (o *Orchestrator) dispatch(ctx context.Context, node Node, job forgejo.WaitingJob) {
	if !o.markDispatching(job.Handle) {
		return
	}
	o.pool.SetState(node.InstanceID, StateBusy)
	o.wg.Go(func() {
		defer func() {
			o.pool.SetState(node.InstanceID, StateIdle)
			o.pool.Touch(node.InstanceID, o.now())
			o.unmarkDispatching(job.Handle)
		}()
		name := o.cfg.Tag + "-" + shortID()
		reg, err := o.jobs.RegisterEphemeral(ctx, name, o.cfg.Labels)
		if err != nil {
			o.log.Error("register ephemeral runner", "err", err)
			return
		}
		o.addActive(reg.UUID)
		defer o.removeActive(reg.UUID)
		if err := o.disp.RunJob(ctx, node.IP, reg, job); err != nil {
			o.log.Error("run job", "handle", job.Handle, "ip", node.IP, "err", err)
			return
		}
		o.log.Info("job complete", "handle", job.Handle, "ip", node.IP)
	})
}

// provisionOne creates a VM, adds it as Provisioning, waits for readiness, then
// marks it Idle. It counts as pending until it lands in the pool so concurrent
// reconciles do not over-provision.
func (o *Orchestrator) provisionOne(ctx context.Context) {
	o.incPending()
	o.wg.Go(func() {
		// When the dispatcher can pre-pin host keys, generate a fresh ed25519 SSH
		// host key per VM and inject its private half via cloud-init so the worker
		// presents exactly this key; the public half is pinned after Provision so
		// even the first dial is verified. A dispatcher without host keys (e.g. a
		// docker-exec one) skips this and renders without a host key.
		pinner, canPin := o.disp.(HostKeyPinner)
		var hostPriv string
		var sshHostPub ssh.PublicKey
		if canPin {
			var err error
			hostPriv, sshHostPub, err = generateHostKey()
			if err != nil {
				o.log.Error("generate worker host key", "err", err)
				o.decPending()
				return
			}
		}
		userData, err := bootstrap.Render(bootstrap.Params{
			RunnerVersion:  o.cfg.RunnerVersion,
			ReadyFile:      o.cfg.ReadyFile,
			HostPrivateKey: hostPriv,
		})
		if err != nil {
			o.log.Error("render cloud-init", "err", err)
			o.decPending()
			return
		}
		spec := provider.Spec{
			Tag:           o.cfg.Tag,
			Name:          o.cfg.Tag + "-" + shortID(),
			UserData:      userData,
			AuthorizedKey: o.cfg.AuthorizedKey,
			Labels:        o.cfg.Labels,
		}
		inst, err := o.prov.Provision(ctx, spec)
		if err != nil {
			o.log.Error("provision", "err", err)
			o.decPending()
			return
		}
		o.pool.Put(&Node{
			InstanceID: inst.ID,
			State:      StateProvisioning,
			IP:         inst.IPv4,
			CreatedAt:  inst.CreatedAt,
			LastBusy:   o.now(),
		})
		o.decPending() // now counted via the pool
		o.log.Info("provisioned", "id", inst.ID, "ip", inst.IPv4)

		// Seed the pin before the first dial so WaitReady's handshake is verified.
		if canPin {
			pinner.PinHostKey(inst.IPv4, sshHostPub)
		}

		if err := o.disp.WaitReady(ctx, inst.IPv4); err != nil {
			o.log.Error("worker readiness", "id", inst.ID, "err", err)
			return // leave it; teardown/orphan sweep will reclaim it
		}
		o.pool.SetState(inst.ID, StateIdle)
		o.log.Info("worker ready", "id", inst.ID)
	})
}

// applyTeardown destroys idle nodes the billing policy says are due.
func (o *Orchestrator) applyTeardown(ctx context.Context) {
	now := o.now()
	for _, n := range o.pool.ByState(StateIdle) {
		if !o.cfg.Teardown.ShouldTeardown(n, now) {
			continue
		}
		if !o.pool.SetState(n.InstanceID, StateRemoving) {
			continue
		}
		id := n.InstanceID
		o.wg.Go(func() {
			if err := o.prov.Destroy(ctx, id); err != nil {
				o.log.Error("destroy", "id", id, "err", err)
				o.pool.SetState(id, StateIdle) // retry next tick
				return
			}
			o.pool.Delete(id)
			o.log.Info("destroyed idle node", "id", id)
		})
	}
}

func (o *Orchestrator) incPending() {
	o.mu.Lock()
	o.pending++
	o.mu.Unlock()
}

func (o *Orchestrator) decPending() {
	o.mu.Lock()
	if o.pending > 0 {
		o.pending--
	}
	o.mu.Unlock()
}

func (o *Orchestrator) pendingCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pending
}

func (o *Orchestrator) addActive(uuid string) {
	o.mu.Lock()
	o.active[uuid] = struct{}{}
	o.mu.Unlock()
}

func (o *Orchestrator) removeActive(uuid string) {
	o.mu.Lock()
	delete(o.active, uuid)
	o.mu.Unlock()
}

func (o *Orchestrator) isActive(uuid string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.active[uuid]
	return ok
}

func (o *Orchestrator) isDispatching(handle string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.dispatching[handle]
	return ok
}

func (o *Orchestrator) markDispatching(handle string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.dispatching[handle]; ok {
		return false
	}
	o.dispatching[handle] = struct{}{}
	return true
}

func (o *Orchestrator) unmarkDispatching(handle string) {
	o.mu.Lock()
	delete(o.dispatching, handle)
	o.mu.Unlock()
}

// filterServiceable keeps jobs whose required labels are all offered by pool.
func filterServiceable(jobs []forgejo.WaitingJob, labels []string) []forgejo.WaitingJob {
	have := map[string]struct{}{}
	for _, l := range labels {
		have[l] = struct{}{}
	}
	var out []forgejo.WaitingJob
	for _, j := range jobs {
		ok := true
		for _, want := range j.Labels {
			if _, has := have[want]; !has {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, j)
		}
	}
	return out
}

// generateHostKey mints a fresh ed25519 SSH host keypair for a worker VM. It
// returns the private key as an OpenSSH-format PEM (for cloud-init injection)
// and the matching ssh.PublicKey (for pinning). The keypair is ephemeral per
// VM; the PEM is never logged.
func generateHostKey() (privPEM string, pub ssh.PublicKey, err error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("generate ed25519 host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return "", nil, fmt.Errorf("marshal host private key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return "", nil, fmt.Errorf("derive host public key: %w", err)
	}
	return string(pem.EncodeToMemory(block)), sshPub, nil
}

func shortID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
