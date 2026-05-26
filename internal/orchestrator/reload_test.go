package orchestrator

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/provider"
)

func baseCfg() Config {
	return Config{
		Tag:           "fj-bellows-test",
		MaxScale:      2,
		Labels:        []string{"ubuntu-latest"},
		PollInterval:  10 * time.Second,
		RunnerVersion: "12.10.1",
		ReadyFile:     "/run/fj-bellows-ready",
		Teardown: TeardownPolicy{
			Model:       provider.BillingPerSecond,
			IdleTimeout: 5 * time.Minute,
			HourMargin:  5 * time.Minute,
			BillingHour: time.Hour,
		},
		AuthorizedKey:   "ssh-ed25519 AAA test",
		DrainOnShutdown: true,
	}
}

func TestApplyHotConfig_NoChangesReturnsEmpty(t *testing.T) {
	o := New(baseCfg(), nil, nil, nil, nil)
	changed, err := o.ApplyHotConfig(baseCfg())
	if err != nil {
		t.Fatalf("ApplyHotConfig: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("changed should be empty, got %v", changed)
	}
}

func TestApplyHotConfig_HotFieldsApplied(t *testing.T) {
	o := New(baseCfg(), nil, nil, nil, nil)
	next := baseCfg()
	next.MaxScale = 8
	next.Labels = []string{"ubuntu-latest", "macos"}
	next.PollInterval = 30 * time.Second
	next.RunnerVersion = "12.11.0"
	next.DrainOnShutdown = false
	next.DrainTimeout = 90 * time.Second
	next.DestroyOnExit = true
	next.Teardown.IdleTimeout = 2 * time.Minute
	next.Teardown.HourMargin = 90 * time.Second
	next.Teardown.BillingHour = 30 * time.Minute

	changed, err := o.ApplyHotConfig(next)
	if err != nil {
		t.Fatalf("ApplyHotConfig: %v", err)
	}
	want := []string{
		"destroy_on_exit",
		"drain_on_shutdown",
		"drain_timeout",
		"forgejo.labels",
		"poll.billing_hour",
		"poll.hour_margin",
		"poll.idle_timeout",
		"poll.interval",
		"runner_version",
		"scale.max",
	}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed:\nwant %v\n got %v", want, changed)
	}

	cur := o.CurrentConfig()
	if cur.MaxScale != 8 {
		t.Errorf("MaxScale not applied: %d", cur.MaxScale)
	}
	if !reflect.DeepEqual(cur.Labels, next.Labels) {
		t.Errorf("Labels not applied: %v", cur.Labels)
	}
	if cur.PollInterval != 30*time.Second {
		t.Errorf("PollInterval not applied: %s", cur.PollInterval)
	}

	// Poll reset signal must have been emitted (PollInterval changed).
	select {
	case d := <-o.pollResetSignal():
		if d != 30*time.Second {
			t.Errorf("pollReset signal: want 30s got %s", d)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected pollReset signal")
	}
}

func TestApplyHotConfig_RejectsNonHot(t *testing.T) {
	o := New(baseCfg(), nil, nil, nil, nil)
	next := baseCfg()
	next.Tag = "different-tag"
	next.ReadyFile = "/run/other"
	next.AuthorizedKey = "ssh-ed25519 AAA other"
	next.Teardown.Model = provider.BillingHourlyRoundUp
	// Mix in a hot change too: must be rejected wholesale, no partial apply.
	next.MaxScale = 99

	_, err := o.ApplyHotConfig(next)
	if err == nil {
		t.Fatal("expected error on non-hot field change")
	}
	msg := err.Error()
	for _, want := range []string{"tag", "ready_file", "ssh.authorized_key", "billing_model"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %s", want, msg)
		}
	}
	// Partial apply guard: MaxScale must NOT have been written.
	if got := o.CurrentConfig().MaxScale; got != 2 {
		t.Errorf("partial apply: MaxScale = %d, want unchanged 2", got)
	}
}

func TestApplyHotConfig_NoPollResetWhenIntervalUnchanged(t *testing.T) {
	o := New(baseCfg(), nil, nil, nil, nil)
	next := baseCfg()
	next.MaxScale = 9 // hot change but not PollInterval
	if _, err := o.ApplyHotConfig(next); err != nil {
		t.Fatalf("ApplyHotConfig: %v", err)
	}
	select {
	case d := <-o.pollResetSignal():
		t.Fatalf("unexpected pollReset signal: %s", d)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHotReloadableListMatchesDiff(t *testing.T) {
	// Build two configs that differ on every hot field at once; the dotted
	// keys returned by diffConfig must be the exported HotReloadable set.
	a := baseCfg()
	b := baseCfg()
	b.MaxScale = a.MaxScale + 1
	b.Labels = append([]string{"extra"}, a.Labels...)
	b.PollInterval = a.PollInterval + time.Second
	b.RunnerVersion = a.RunnerVersion + "-rc1"
	b.DrainOnShutdown = !a.DrainOnShutdown
	b.DrainTimeout = a.DrainTimeout + time.Second
	b.DestroyOnExit = !a.DestroyOnExit
	b.Teardown.IdleTimeout = a.Teardown.IdleTimeout + time.Second
	b.Teardown.HourMargin = a.Teardown.HourMargin + time.Second
	b.Teardown.BillingHour = a.Teardown.BillingHour + time.Second

	changed, blocked := diffConfig(a, b)
	if len(blocked) != 0 {
		t.Fatalf("unexpected blocked fields: %v", blocked)
	}

	got := map[string]bool{}
	for _, k := range changed {
		got[k] = true
	}
	for _, k := range HotReloadable {
		if !got[k] {
			t.Errorf("HotReloadable lists %q but diff did not surface it", k)
		}
	}
	for k := range got {
		if !slices.Contains(HotReloadable, k) {
			t.Errorf("diff surfaced %q but it is not in HotReloadable", k)
		}
	}
}
