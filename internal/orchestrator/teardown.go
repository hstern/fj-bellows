package orchestrator

import (
	"time"

	"github.com/hstern/fj-bellows/internal/provider"
)

// TeardownPolicy holds the timers that drive idle teardown.
type TeardownPolicy struct {
	Model       provider.BillingModel
	IdleTimeout time.Duration // per-second model
	HourMargin  time.Duration // hourly model (5m -> the :55 rule)
}

// ShouldTeardown reports whether an idle node should be torn down now.
//
//   - Per-second billing: tear down once idle for IdleTimeout.
//   - Hourly rounding: tear down once past the kill mark for the current paid
//     hour (creation + N*hour - HourMargin), so the DELETE finishes before the
//     next hour bills. A busy node simply rolls into the next paid hour and is
//     re-evaluated when it returns to idle.
func (tp TeardownPolicy) ShouldTeardown(n Node, now time.Time) bool {
	switch tp.Model {
	case provider.BillingPerSecond:
		return now.Sub(n.LastBusy) >= tp.IdleTimeout
	case provider.BillingHourlyRoundUp:
		return !now.Before(NextKillMark(n.CreatedAt, now, tp.HourMargin))
	default:
		return false
	}
}

// NextKillMark returns the idle-kill instant for the paid hour that now falls
// in: creation + (completedHours+1)*hour - margin. For a node created at T this
// yields T+55m in the first hour, T+115m in the second, and so on (margin 5m).
func NextKillMark(created, now time.Time, margin time.Duration) time.Time {
	if now.Before(created) {
		now = created
	}
	completed := int64(now.Sub(created) / time.Hour)
	return created.Add(time.Duration(completed+1)*time.Hour - margin)
}
