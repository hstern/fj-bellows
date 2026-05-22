package forgejo

// WaitingJob is a job sitting in the Actions queue, returned by
// GET /api/v1/<scope>/actions/runners/jobs.
//
// NOTE: the exact JSON field names should be confirmed against the live
// Forgejo (>= v15.0) API; they are decoded leniently and unknown fields are
// ignored. Handle is the per-attempt token that one-job --handle consumes.
type WaitingJob struct {
	ID     int64    `json:"id"`
	Handle string   `json:"handle"`
	Labels []string `json:"labels"`
	Status string   `json:"status"`
}

// Registration is the result of registering an ephemeral runner.
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
