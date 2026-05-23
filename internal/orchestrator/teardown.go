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

	// BillingHour is the cycle length used by the hourly model to compute the
	// next kill mark (kill = created + N*BillingHour - HourMargin). Defaults
	// to 1h when zero, matching every cloud's actual hourly rounding. Tests
	// and operators who want faster reclamation can override it; the provider
	// still bills its real hourly rate regardless of what we pick here.
	BillingHour time.Duration
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
		return !now.Before(NextKillMark(n.CreatedAt, now, tp.cycle(), tp.HourMargin))
	default:
		return false
	}
}

// cycle returns the billing cycle length, falling back to one hour so existing
// callers/tests that leave BillingHour zero keep their original semantics.
func (tp TeardownPolicy) cycle() time.Duration {
	if tp.BillingHour > 0 {
		return tp.BillingHour
	}
	return time.Hour
}

// NextKillMark returns the idle-kill instant for the paid cycle that now falls
// in: creation + (completedCycles+1)*cycle - margin. For a node created at T
// with a 1h cycle and 5m margin this yields T+55m in the first hour, T+115m in
// the second, and so on. With a 5m cycle and 2m margin it yields T+3m, T+8m,
// T+13m, etc.
func NextKillMark(created, now time.Time, cycle, margin time.Duration) time.Time {
	if cycle <= 0 {
		cycle = time.Hour
	}
	if now.Before(created) {
		now = created
	}
	completed := int64(now.Sub(created) / cycle)
	return created.Add(time.Duration(completed+1)*cycle - margin)
}
