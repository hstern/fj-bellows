package orchestrator

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// HotReloadable lists the orchestrator config fields that ApplyHotConfig will
// swap in without restarting the daemon. Every other field on Config requires
// a process restart because it's plumbed into something the orchestrator
// builds once at startup (provider client, dispatcher, Forgejo client,
// signing key, billing model).
//
// The list is exported so the README and any future operator-facing surface
// can render the authoritative set without drifting from the implementation.
var HotReloadable = []string{
	"scale.max",
	"forgejo.labels",
	"poll.interval",
	"poll.idle_timeout",
	"poll.hour_margin",
	"poll.billing_hour",
	"runner_version",
	"drain_on_shutdown",
	"drain_timeout",
	"destroy_on_exit",
}

// ApplyHotConfig computes the diff between the orchestrator's current
// config and newCfg, rejects diffs that touch non-hot fields, and atomically
// installs the new values. Returns the list of changed dotted-key field
// names so the caller can surface them to operators.
//
// When PollInterval changes, the running goroutine's ticker is reset via a
// signal on the pollChanged channel so the new cadence takes effect on the
// next tick boundary. This is the only field that requires touching live
// state outside o.cfg.
//
// The orchestrator's reconcile loop reads o.cfg under o.mu on each tick, so
// holding o.mu for the swap is enough to make new values visible to the
// next reconcile without tearing.
func (o *Orchestrator) ApplyHotConfig(newCfg Config) ([]string, error) {
	o.mu.Lock()
	cur := o.cfg
	o.mu.Unlock()

	changed, blocked := diffConfig(cur, newCfg)
	if len(blocked) > 0 {
		sort.Strings(blocked)
		return nil, fmt.Errorf("reload rejected: %d non-hot field(s) changed (restart required): %s",
			len(blocked), strings.Join(blocked, ", "))
	}
	if len(changed) == 0 {
		return nil, nil
	}

	o.mu.Lock()
	pollChanged := newCfg.PollInterval != o.cfg.PollInterval
	newInterval := newCfg.PollInterval
	o.cfg = newCfg
	o.mu.Unlock()

	if pollChanged && o.pollReset != nil {
		// Non-blocking: if Run has not yet drained a previous signal,
		// the latest interval is the one that lands (the receiver reads
		// o.cfg.PollInterval anyway, the signal just nudges it).
		select {
		case o.pollReset <- newInterval:
		default:
		}
	}

	sort.Strings(changed)
	return changed, nil
}

// ErrReloadBlocked is the sentinel returned by ApplyHotConfig when the new
// config touches a non-hot field. Callers (typically the control handler)
// should map it to CodeFailedPrecondition.
var ErrReloadBlocked = errors.New("reload blocked: non-hot field changed")

// diffConfig walks the two configs and returns (changed-hot-fields,
// blocked-non-hot-fields). Field name conventions follow the on-disk YAML
// schema (config.yaml) so operators see names they can locate in their own
// config file.
func diffConfig(a, b Config) (changed, blocked []string) {
	return hotFieldDiff(a, b), nonHotFieldDiff(a, b)
}

// hotFieldDiff lists the dotted names of hot-reloadable fields whose values
// differ between a and b.
func hotFieldDiff(a, b Config) []string {
	var out []string
	if a.MaxScale != b.MaxScale {
		out = append(out, "scale.max")
	}
	if !stringSliceEqual(a.Labels, b.Labels) {
		out = append(out, "forgejo.labels")
	}
	if a.PollInterval != b.PollInterval {
		out = append(out, "poll.interval")
	}
	if a.RunnerVersion != b.RunnerVersion {
		out = append(out, "runner_version")
	}
	if a.DrainOnShutdown != b.DrainOnShutdown {
		out = append(out, "drain_on_shutdown")
	}
	if a.DrainTimeout != b.DrainTimeout {
		out = append(out, "drain_timeout")
	}
	if a.DestroyOnExit != b.DestroyOnExit {
		out = append(out, "destroy_on_exit")
	}
	if a.Teardown.IdleTimeout != b.Teardown.IdleTimeout {
		out = append(out, "poll.idle_timeout")
	}
	if a.Teardown.HourMargin != b.Teardown.HourMargin {
		out = append(out, "poll.hour_margin")
	}
	if a.Teardown.BillingHour != b.Teardown.BillingHour {
		out = append(out, "poll.billing_hour")
	}
	return out
}

// nonHotFieldDiff lists the dotted names of restart-required fields whose
// values differ between a and b.
func nonHotFieldDiff(a, b Config) []string {
	var out []string
	if a.Tag != b.Tag {
		out = append(out, "tag")
	}
	if a.ReadyFile != b.ReadyFile {
		out = append(out, "ready_file")
	}
	if a.AuthorizedKey != b.AuthorizedKey {
		out = append(out, "ssh.authorized_key")
	}
	if a.Teardown.Model != b.Teardown.Model {
		out = append(out, "billing_model")
	}
	return out
}

func stringSliceEqual(a, b []string) bool {
	return reflect.DeepEqual(a, b)
}

// CurrentConfig returns a copy of the orchestrator's live runtime config.
// Used by the control-plane backend on reload to overlay only the hot-fields
// without losing CLI-flag-derived values (RunnerVersion, drain settings, …)
// that the on-disk YAML never owned. Returning a copy keeps callers from
// reaching into o.cfg without taking o.mu.
func (o *Orchestrator) CurrentConfig() Config {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg
}

// pollResetSignal is exposed for the Run goroutine so it can subscribe to
// poll-interval changes from ApplyHotConfig. Returns nil before Run wires the
// channel (defensive — no caller should subscribe before Run starts).
func (o *Orchestrator) pollResetSignal() <-chan time.Duration { return o.pollReset }
