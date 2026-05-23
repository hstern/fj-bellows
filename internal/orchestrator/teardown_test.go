package orchestrator

import (
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/provider"
)

func TestNextKillMark(t *testing.T) {
	created := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		cycle  time.Duration
		margin time.Duration
		now    time.Time
		want   time.Time
	}{
		// 1h / 5m: the classic ":55" rule.
		{"1h start of first hour", time.Hour, 5 * time.Minute, created, created.Add(55 * time.Minute)},
		{"1h mid first hour", time.Hour, 5 * time.Minute, created.Add(30 * time.Minute), created.Add(55 * time.Minute)},
		{"1h into second hour", time.Hour, 5 * time.Minute, created.Add(65 * time.Minute), created.Add(115 * time.Minute)},
		{"1h into third hour", time.Hour, 5 * time.Minute, created.Add(125 * time.Minute), created.Add(175 * time.Minute)},

		// 5m / 2m: the user's worked example — kill at created+3m, +8m, +13m...
		{"5m start of first cycle", 5 * time.Minute, 2 * time.Minute, created, created.Add(3 * time.Minute)},
		{"5m mid first cycle", 5 * time.Minute, 2 * time.Minute, created.Add(2 * time.Minute), created.Add(3 * time.Minute)},
		{"5m into second cycle", 5 * time.Minute, 2 * time.Minute, created.Add(6 * time.Minute), created.Add(8 * time.Minute)},
		{"5m into third cycle", 5 * time.Minute, 2 * time.Minute, created.Add(11 * time.Minute), created.Add(13 * time.Minute)},

		// Zero cycle falls back to 1h (defensive default).
		{"zero cycle falls back to 1h", 0, 5 * time.Minute, created, created.Add(55 * time.Minute)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextKillMark(created, tc.now, tc.cycle, tc.margin); !got.Equal(tc.want) {
				t.Errorf("NextKillMark(cycle=%s, margin=%s) = %s, want %s",
					tc.cycle, tc.margin, got, tc.want)
			}
		})
	}
}

func TestShouldTeardownHourly(t *testing.T) {
	created := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		cycle   time.Duration
		margin  time.Duration
		elapsed time.Duration
		want    bool
	}{
		// 1h / 5m: the classic case. Leaving BillingHour zero must keep this
		// semantics for backward compatibility with existing callers.
		{"1h: warm well before kill mark", 0, 5 * time.Minute, 30 * time.Minute, false},
		{"1h: just before kill mark", 0, 5 * time.Minute, 54 * time.Minute, false},
		{"1h: at kill mark", 0, 5 * time.Minute, 55 * time.Minute, true},
		{"1h: past kill mark", 0, 5 * time.Minute, 56 * time.Minute, true},
		{"1h: rolled into next paid hour", 0, 5 * time.Minute, 65 * time.Minute, false},
		{"1h: kill mark of second hour", 0, 5 * time.Minute, 115 * time.Minute, true},

		// Same with BillingHour explicitly set (proves the explicit form matches).
		{"1h explicit: at kill mark", time.Hour, 5 * time.Minute, 55 * time.Minute, true},

		// 5m / 2m: the user's worked example.
		{"5m/2m: warm well before kill mark", 5 * time.Minute, 2 * time.Minute, 1 * time.Minute, false},
		{"5m/2m: just before kill mark", 5 * time.Minute, 2 * time.Minute, 2*time.Minute + 59*time.Second, false},
		{"5m/2m: at first kill mark", 5 * time.Minute, 2 * time.Minute, 3 * time.Minute, true},
		{"5m/2m: past first kill mark", 5 * time.Minute, 2 * time.Minute, 4 * time.Minute, true},
		{"5m/2m: rolled into second cycle", 5 * time.Minute, 2 * time.Minute, 6 * time.Minute, false},
		{"5m/2m: at second kill mark", 5 * time.Minute, 2 * time.Minute, 8 * time.Minute, true},
		{"5m/2m: at third kill mark", 5 * time.Minute, 2 * time.Minute, 13 * time.Minute, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tp := TeardownPolicy{
				Model:       provider.BillingHourlyRoundUp,
				HourMargin:  c.margin,
				BillingHour: c.cycle,
			}
			n := Node{CreatedAt: created, State: StateIdle}
			now := created.Add(c.elapsed)
			if got := tp.ShouldTeardown(n, now); got != c.want {
				t.Errorf("elapsed %s (cycle %s, margin %s): ShouldTeardown = %v, want %v",
					c.elapsed, c.cycle, c.margin, got, c.want)
			}
		})
	}
}

func TestShouldTeardownPerSecond(t *testing.T) {
	tp := TeardownPolicy{Model: provider.BillingPerSecond, IdleTimeout: 5 * time.Minute}
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	n := Node{LastBusy: base, State: StateIdle}

	if tp.ShouldTeardown(n, base.Add(4*time.Minute)) {
		t.Error("should stay alive before idle timeout")
	}
	if !tp.ShouldTeardown(n, base.Add(5*time.Minute)) {
		t.Error("should tear down at idle timeout")
	}
	if !tp.ShouldTeardown(n, base.Add(10*time.Minute)) {
		t.Error("should tear down well past idle timeout")
	}
}

func TestShouldTeardownUnknownModel(t *testing.T) {
	tp := TeardownPolicy{Model: provider.BillingModel(99)}
	if tp.ShouldTeardown(Node{}, time.Now()) {
		t.Error("unknown billing model must not tear down")
	}
}
