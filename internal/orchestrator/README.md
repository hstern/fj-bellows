# internal/orchestrator

The always-on daemon core: the poll/reconcile loop, the node state machine, the
billing-model-aware teardown policy, and the SSH job dispatcher.

## Node state machine (`state.go`)

```
Provisioning -> Idle -> Busy -> Idle -> Draining -> Removing
```

- **Provisioning** — VM created, awaiting SSH readiness.
- **Idle** — ready and warm, no job assigned.
- **Busy** — a `one-job` run is in flight.
- **Draining / Removing** — teardown decided / `Destroy` issued.

`Pool` is the concurrency-safe node set. Stored nodes are copied in/out so
callers can't mutate shared state.

## Reconcile (`orchestrator.go`)

Each tick, under the single reconcile goroutine:

1. `provider.List(tag)` — ground truth. Adopt unknown instances (crash
   recovery, rebuilding billing timers from `CreatedAt`); drop vanished ones
   (Provisioning nodes are never dropped — a fresh VM may not be listed yet).
2. `WaitingJobs` — filter to jobs whose required labels this pool offers.
3. Dispatch each serviceable job to an Idle node; provision for the rest, capped
   by `MaxScale` (in-flight provisions count as `pending` so concurrent ticks
   don't over-provision).
4. Apply teardown to Idle nodes.
5. Reap zombie runners: `ListRunners` and delete any registration whose name
   carries our tag prefix but that we aren't currently running a job for (a VM
   that died after registering but before `one-job` finished). Deletion requires
   the runner to look orphaned for two consecutive ticks, closing the race
   against a freshly registered runner. This is the third reconcile source
   (Forgejo jobs + Forgejo runners + provider instances).

The reconcile loop is the **single writer of provisioning decisions**;
dispatch/teardown goroutines mutate only their own node's state.

## Teardown (`teardown.go`)

- `BillingPerSecond` — tear down after `IdleTimeout`.
- `BillingHourlyRoundUp` — tear down at the kill mark
  `created + (completedHours+1)*hour - HourMargin` (the `:55` rule). Timers are
  **derived from `CreatedAt` each tick**, not stored, so they survive restarts.

## Shutdown

Jobs run under a context independent of the shutdown signal, so `Run` can choose
how to stop. With `DrainOnShutdown` (default) it stops scheduling and waits for
in-flight goroutines to finish (bounded by `DrainTimeout`, `0` = wait
indefinitely); otherwise it cancels the job context to interrupt them. With
`DestroyOnExit` it tears down all owned VMs on the way out — default leaves warm
VMs for a restarted daemon to readopt. In-flight goroutines are tracked with a
`WaitGroup` (`wg.Go`).

## Dispatch (`dispatch.go`)

`Dispatcher` is an interface so the orchestrator is unit-testable without real
SSH **and so the dispatch mechanism can be selected per provider**. Its methods
take both the worker's provider `id` and its network `addr`: SSH dispatch uses
`addr`, while a mechanism like docker exec uses `id`. `SSHDispatcher` connects
in-process with `golang.org/x/crypto/ssh`, writes the one-shot token via stdin
(never the command line), and runs `forgejo-runner one-job ... --wait` to
completion. The composition root (`cmd/fj-bellows`) injects the dispatcher, so a
provider whose workers aren't reached over SSH supplies a different one.

Host keys are verified with **trust-on-first-use (TOFU) per-VM pinning**: fresh
per-hour VMs have no pre-known host key, so the first successful handshake to an
address records the presented key in an in-dispatcher pin store, and every later
dial to that address must present a byte-equal key (a mismatch is rejected as a
possible MITM). Different addresses are pinned independently. Residual risk: a
MITM present at the very first contact could still impersonate the VM; after
that, its identity is verified for the rest of its life.

Dependencies are interfaces (`JobSource`, `Dispatcher`, `provider.Provider`);
see [`mock`](mock) for the test doubles.
