# internal/forgejo

A thin REST client for the Forgejo Actions runner API, scoped to one runner
owner (`orgs/<org>` or `repos/<owner>/<name>`).

- `WaitingJobs` — `GET /api/v1/<scope>/actions/runners/jobs`; returns queued
  jobs with their required labels and per-attempt handle. Tolerates both a bare
  array and a `{"jobs":[...]}` envelope.
- `RegisterEphemeral` — `POST /api/v1/<scope>/actions/runners` with
  `{"ephemeral":true}`; returns the one-shot `uuid` + `token`. Forgejo
  invalidates these and removes the registration after a single job.

The client authenticates with the admin token (`Authorization: token <token>`).

> The exact job JSON field names should be confirmed against the live Forgejo
> (≥ v15.0) API; unknown fields are ignored.
