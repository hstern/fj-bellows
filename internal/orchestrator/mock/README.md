# internal/orchestrator/mock

Hand-written mocks of the orchestrator's dependency interfaces for unit tests.

- `JobSource` — mocks `orchestrator.JobSource` (`WaitingJobs`,
  `RegisterEphemeral`). `RegisterEphemeral` defaults to returning a dummy
  registration so dispatch tests work without configuration.
- `Dispatcher` — mocks `orchestrator.Dispatcher` (`WaitReady`, `RunJob`).

Methods delegate to function fields and record calls (`RegisterCount`,
`RunCount`) for assertions; both are safe for concurrent use because the
orchestrator dispatches from goroutines.

These mocks import only `internal/forgejo` (not `orchestrator`), so they satisfy
the interfaces structurally without an import cycle.
