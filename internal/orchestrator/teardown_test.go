package orchestrator

import (
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/provider"
)

func TestNextKillMark(t *testing.T) {
	created := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	margin := 5 * time.Minute
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"start of first hour", created, created.Add(55 * time.Minute)},
		{"mid first hour", created.Add(30 * time.Minute), created.Add(55 * time.Minute)},
		{"into second hour", created.Add(65 * time.Minute), created.Add(115 * time.Minute)},
		{"into third hour", created.Add(125 * time.Minute), created.Add(175 * time.Minute)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextKillMark(created, tc.now, margin); !got.Equal(tc.want) {
				t.Errorf("NextKillMark = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestShouldTeardownHourly(t *testing.T) {
	created := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	tp := TeardownPolicy{Model: provider.BillingHourlyRoundUp, HourMargin: 5 * time.Minute}
	n := Node{CreatedAt: created, State: StateIdle}

	cases := []struct {
		elapsed time.Duration
		want    bool
	}{
		{30 * time.Minute, false}, // warm, well before :55
		{54 * time.Minute, false}, // just before kill mark
		{55 * time.Minute, true},  // at :55 -> kill
		{56 * time.Minute, true},  // past :55
		{65 * time.Minute, false}, // rolled into next paid hour, warm again
		{115 * time.Minute, true}, // :55 of the second hour
	}
	for _, c := range cases {
		now := created.Add(c.elapsed)
		if got := tp.ShouldTeardown(n, now); got != c.want {
			t.Errorf("elapsed %s: ShouldTeardown = %v, want %v", c.elapsed, got, c.want)
		}
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
