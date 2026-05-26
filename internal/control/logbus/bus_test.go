package logbus_test

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/control/logbus"
)

const testInstance = "vm-1"

func TestBus_PublishFansOutToSubscribers(t *testing.T) {
	b := logbus.New()
	chA, cancelA := b.Subscribe()
	defer cancelA()
	chB, cancelB := b.Subscribe()
	defer cancelB()

	b.Publish(logbus.Record{Message: "hello", Level: slog.LevelInfo, At: time.Now()})

	for name, ch := range map[string]<-chan logbus.Record{"A": chA, "B": chB} {
		select {
		case r := <-ch:
			if r.Message != "hello" {
				t.Fatalf("%s got %q", name, r.Message)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not receive", name)
		}
	}
}

func TestBus_CancelClosesChannelAndIsIdempotent(t *testing.T) {
	b := logbus.New()
	ch, cancel := b.Subscribe()
	cancel()
	cancel() // must not panic

	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
	if got := b.Subscribers(); got != 0 {
		t.Fatalf("Subscribers after cancel: want 0 got %d", got)
	}
	// Publish after cancel must not panic, must not deliver.
	b.Publish(logbus.Record{Message: "post-cancel"})
}

func TestBus_DropsSlowSubscribers(t *testing.T) {
	b := logbus.New()
	ch, _ := b.Subscribe() // never read

	for range logbus.SubscriberBuffer {
		b.Publish(logbus.Record{Message: "flood"})
	}
	if got := b.Subscribers(); got != 1 {
		t.Fatalf("after buffer fill: want 1 subscriber got %d", got)
	}
	b.Publish(logbus.Record{Message: "overflow"})
	if got := b.Subscribers(); got != 0 {
		t.Fatalf("after overflow: want 0 subscribers got %d", got)
	}
	// Drain ch; values received first, then channel closes.
	drained := 0
	deadline := time.After(time.Second)
DrainLoop:
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				break DrainLoop
			}
			drained++
		case <-deadline:
			t.Fatal("channel never closed after drop")
		}
	}
	if drained != logbus.SubscriberBuffer {
		t.Fatalf("drained: want %d got %d", logbus.SubscriberBuffer, drained)
	}
}

func TestBus_HistoryBoundedToCapacity(t *testing.T) {
	b := logbus.New()
	// Publish more than HistoryCapacity records.
	const extra = 50
	total := logbus.HistoryCapacity + extra
	for i := range total {
		b.Publish(logbus.Record{Message: "msg", Attrs: map[string]string{"i": itoa(i)}})
	}

	all := b.History(logbus.HistoryCapacity*2, logbus.Filter{})
	if len(all) != logbus.HistoryCapacity {
		t.Fatalf("History full: want %d got %d", logbus.HistoryCapacity, len(all))
	}
	// Should be the tail: oldest first index is `extra` (i = 50).
	if got := all[0].Attrs["i"]; got != itoa(extra) {
		t.Fatalf("oldest record i: want %s got %s", itoa(extra), got)
	}
	if got := all[len(all)-1].Attrs["i"]; got != itoa(total-1) {
		t.Fatalf("newest record i: want %s got %s", itoa(total-1), got)
	}
}

func TestBus_HistoryReturnsLastN(t *testing.T) {
	b := logbus.New()
	for i := range 20 {
		b.Publish(logbus.Record{Attrs: map[string]string{"i": itoa(i)}})
	}
	got := b.History(5, logbus.Filter{})
	if len(got) != 5 {
		t.Fatalf("History(5): want 5 got %d", len(got))
	}
	// Most recent 5 -> i in [15,16,17,18,19].
	for k, want := range []int{15, 16, 17, 18, 19} {
		if got[k].Attrs["i"] != itoa(want) {
			t.Fatalf("History[%d].i: want %d got %s", k, want, got[k].Attrs["i"])
		}
	}
}

func TestBus_HistoryFilter(t *testing.T) {
	b := logbus.New()
	b.Publish(logbus.Record{Message: "a", Attrs: map[string]string{"id": testInstance}})
	b.Publish(logbus.Record{Message: "b", Attrs: map[string]string{"id": "vm-2"}})
	b.Publish(logbus.Record{Message: "c", Attrs: map[string]string{"id": testInstance, "handle": "h1"}})
	b.Publish(logbus.Record{Message: "d", Attrs: map[string]string{"handle": "h2"}})

	got := b.History(100, logbus.Filter{InstanceID: testInstance})
	if len(got) != 2 {
		t.Fatalf("filter id=vm-1: want 2 got %d", len(got))
	}
	if got[0].Message != "a" || got[1].Message != "c" {
		t.Fatalf("filter id=vm-1 wrong matches: %+v", got)
	}

	got = b.History(100, logbus.Filter{Handle: "h2"})
	if len(got) != 1 || got[0].Message != "d" {
		t.Fatalf("filter handle=h2: got %+v", got)
	}

	got = b.History(100, logbus.Filter{InstanceID: testInstance, Handle: "h1"})
	if len(got) != 1 || got[0].Message != "c" {
		t.Fatalf("filter id+handle: got %+v", got)
	}

	got = b.History(100, logbus.Filter{InstanceID: "nope"})
	if len(got) != 0 {
		t.Fatalf("filter nope: want empty got %+v", got)
	}
}

func TestBus_HistoryZeroOrNegativeN(t *testing.T) {
	b := logbus.New()
	b.Publish(logbus.Record{Message: "x"})
	if got := b.History(0, logbus.Filter{}); got != nil {
		t.Fatalf("History(0): want nil got %+v", got)
	}
	if got := b.History(-1, logbus.Filter{}); got != nil {
		t.Fatalf("History(-1): want nil got %+v", got)
	}
}

func TestBus_SubscribeFiltered(t *testing.T) {
	b := logbus.New()
	ch, cancel := b.SubscribeFiltered(logbus.Filter{InstanceID: testInstance})
	defer cancel()

	// Non-matching record: bus should NOT deliver, and channel should not
	// be filled by it.
	b.Publish(logbus.Record{Attrs: map[string]string{"id": "vm-2"}})
	b.Publish(logbus.Record{Message: "want", Attrs: map[string]string{"id": testInstance}})

	select {
	case r := <-ch:
		if r.Message != "want" {
			t.Fatalf("got non-matching record through filter: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive matching record")
	}
	// Ensure we got exactly one delivery.
	select {
	case r := <-ch:
		t.Fatalf("unexpected extra delivery: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBus_ConcurrentPublishAndSubscribe(_ *testing.T) {
	b := logbus.New()
	const subs = 20
	const recordsPerSub = 50

	var wg sync.WaitGroup
	wg.Add(subs)
	for range subs {
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe()
			defer cancel()
			seen := 0
			deadline := time.After(2 * time.Second)
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
					seen++
					if seen >= recordsPerSub {
						return
					}
				case <-deadline:
					return
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	for range recordsPerSub * 2 {
		b.Publish(logbus.Record{Message: "tick"})
	}
	wg.Wait()
}

// itoa avoids strconv import bloat for tiny tests.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
