# internal/control/logbus

In-process publish/subscribe bus + bounded ring buffer for structured slog
records. Powers the `StreamLogs` ConnectRPC so operators can tail the daemon
over the control plane instead of `ssh ... tail -f`.

## What's here

- **`Bus`** — fan-out to N subscribers. Each subscriber gets a buffered
  channel (`SubscriberBuffer = 256`); the bus drops slow subscribers
  (close + remove) so the producer never blocks. Mirrors the design of
  `internal/control/events`.
- **Ring buffer** — the bus also keeps the last `HistoryCapacity = 1000`
  records so a new `StreamLogs` subscriber can request a replay of the
  most recent N records before live streaming begins. `History(n, filter)`
  returns up to n matching records in chronological order (oldest first);
  if the buffer holds more than n matches, only the most recent n come
  back.
- **`Filter`** — optional `{InstanceID, Handle}` scoping, matched against
  `Record.Attrs["id"]` and `Record.Attrs["handle"]`. Empty fields mean no
  filter on that dimension. Used by `SubscribeFiltered` and `History`.
- **`Handler`** — a `slog.Handler` wrapper. The daemon's main builds it as
  `slog.New(logbus.NewHandler(textHandler, bus))`; every record then
  reaches both stderr (via the wrapped text handler) AND the bus (for
  StreamLogs subscribers). `WithAttrs` / `WithGroup` are honoured —
  group-prefixed keys land as `parent.child` in `Record.Attrs`.

## Why both a bus AND a ring buffer

`StreamEvents` (sibling `events/` package) is push-only — operators
connect, then see what fires. Logs are different: the operator opening a
debug session almost always wants the records that fired _just before_
they connected. The ring buffer makes that history available without
adding disk persistence or a separate journal.

## Attribute flattening

`Record.Attrs` is `map[string]string`. `Handler.toRecord` flattens slog's
typed attributes:

- string values pass through.
- non-string scalars (int, bool, duration, time, error, …) stringify via
  `slog.Value.String()`.
- group-valued attrs (`slog.Group("billing", ...)`) recurse with the
  group name as a `.`-separated prefix (`billing.model`, `billing.margin`).
- pre-applied attrs from `WithAttrs` sit at the parent level; group
  prefixes from `WithGroup` apply to in-record attrs only.

## Drop policy

Identical to `events/`: if a subscriber's 256-record buffer fills, the bus
closes its channel and removes it from the set. The `StreamLogs` handler
treats `ok=false` from the channel as a `CodeResourceExhausted` and
returns to the client, who can reconnect.

## Tests

`bus_test.go` covers fan-out, idempotent cancel, slow-subscriber drop,
ring-buffer cap, History filter (positive + negative), and concurrent
publish/subscribe. `handler_test.go` pins delegation (records reach BOTH
the next handler and the bus), `WithAttrs`, `WithGroup`, `slog.Group`
attrs, and `Enabled` propagation.
