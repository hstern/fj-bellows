package forgejo

// WaitingJob is a job sitting in the Actions queue, returned by
// GET /api/v1/<scope>/actions/runners/jobs?labels=...
//
// Field names match the Forgejo 11.x/12.x ActionRunJob response (the live API
// returns a bare JSON array of these). The endpoint REQUIRES a non-empty
// `labels` query parameter — without it Forgejo returns `null` rather than an
// empty array. Client.WaitingJobs supplies the pool labels automatically.
//
// Handle is the per-attempt token consumed by `forgejo-runner one-job
// --handle`, which is a Forgejo 15+ feature. It is decoded leniently (Forgejo
// 12 omits the field) so existing call sites continue to compile against
// Forgejo 12 instances; the orchestrator falls back to the job ID as a stable
// per-job key when Handle is empty.
type WaitingJob struct {
	ID     int64    `json:"id"`
	Handle string   `json:"handle"`
	Labels []string `json:"runs_on"`
	Status string   `json:"status"`
	TaskID int64    `json:"task_id"`
	Name   string   `json:"name"`
}

// Registration is the result of registering an ephemeral runner.
//
// NOTE: the REST POST /actions/runners endpoint that returns this shape is a
// Forgejo 15+ feature; Forgejo <= 12 only exposes
// /actions/runners/registration-token (a shared secret consumed by
// `forgejo-runner register`). RegisterEphemeral returns a descriptive error
// when the server does not implement the ephemeral REST registration so the
// caller can surface a clear "needs Forgejo 15+" diagnostic rather than a
// generic 404.
type Registration struct {
	UUID  string `json:"uuid"`
	Token string `json:"token"`
}

// Runner is a registered Actions runner, returned by
// GET /api/v1/<scope>/actions/runners. Field names should be confirmed against
// the live API; unknown fields are ignored.
type Runner struct {
	ID     int64  `json:"id"`
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}
