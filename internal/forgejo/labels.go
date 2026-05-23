package forgejo

import "strings"

// runner-label schemes that forgejo-runner recognises to bind a label to a
// job-container backend. Anything else after a colon is treated as part of the
// label name (some hostnames legitimately contain colons, though that's rare).
var labelBindingSchemes = []string{
	"docker://",
	"host://",
	"kubernetes://",
}

// SplitLabel splits a runner label of the form "<name>:<scheme>://<value>"
// (e.g. "ubuntu-latest:docker://catthehacker/ubuntu:act-latest") into its bare
// name and the binding string. A label with no recognised scheme is returned
// as (label, ""). See #39 for why this split matters: Forgejo's job-queue API
// and the orchestrator's job-matching want the bare name, while the
// ephemeral-runner registration and the worker's `forgejo-runner one-job
// --label` argument want the full binding so the runner can pick the right
// job-container image.
func SplitLabel(s string) (name, binding string) {
	before, after, ok := strings.Cut(s, ":")
	if !ok {
		return s, ""
	}
	for _, scheme := range labelBindingSchemes {
		if strings.HasPrefix(after, scheme) {
			return before, after
		}
	}
	return s, ""
}

// BareLabels strips any binding from each label, returning just the names.
// Used for the WaitingJobs `?labels=` filter and pool-vs-job matching, where
// the comparison target is the bare label a workflow declared in `runs_on`.
func BareLabels(labels []string) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i], _ = SplitLabel(l)
	}
	return out
}
