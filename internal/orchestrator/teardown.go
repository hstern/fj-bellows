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

// Billing-model strings exposed by Timing.BillingModel. Stable wire constants
// for the operator-facing control plane (FJB-30); not the same surface as
// provider.BillingModel.String() (which is human-readable with a hyphen) —
// these are snake_case so JSON/CLI consumers can switch on them cleanly.
const (
	billingModelPerSecond     = "per_second"
	billingModelHourlyRoundUp = "hourly_round_up"
)

// TeardownTiming is the per-worker billing-window snapshot the control
// plane returns to operators. Fields default to zero/empty when the policy
// model doesn't make them meaningful.
type TeardownTiming struct {
	// PaidHourEndAt is the next paid-hour boundary for the worker — when
	// hourly-round-up billing models would close out the next paid hour.
	// Zero for per-second models.
	PaidHourEndAt time.Time
	// ReapEligibleAt is when the worker first becomes eligible for
	// teardown under the current policy: LastBusy + IdleTimeout for
	// per-second, the next :55 mark for hourly.
	ReapEligibleAt time.Time
	// BillingModel is the policy's billing model string:
	// "per_second" | "hourly_round_up".
	BillingModel string
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

// Timing returns the teardown-timing snapshot for a node under the current
// policy. ShouldTeardown is unchanged; this is a read-only sibling that
// exposes the intermediate computation for operator-facing surfaces (the
// control plane's billing-window view, FJB-30).
func (tp TeardownPolicy) Timing(n Node, now time.Time) TeardownTiming {
	switch tp.Model {
	case provider.BillingPerSecond:
		var reap time.Time
		if !n.LastBusy.IsZero() {
			reap = n.LastBusy.Add(tp.IdleTimeout)
		}
		return TeardownTiming{
			ReapEligibleAt: reap,
			BillingModel:   billingModelPerSecond,
		}
	case provider.BillingHourlyRoundUp:
		var paidEnd, reap time.Time
		if !n.CreatedAt.IsZero() {
			cycle := tp.cycle()
			reap = NextKillMark(n.CreatedAt, now, cycle, tp.HourMargin)
			paidEnd = reap.Add(tp.HourMargin)
		}
		return TeardownTiming{
			PaidHourEndAt:  paidEnd,
			ReapEligibleAt: reap,
			BillingModel:   billingModelHourlyRoundUp,
		}
	default:
		return TeardownTiming{}
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
