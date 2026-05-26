package events_test

import (
	"sync"
	"testing"
	"time"

	"github.com/hstern/fj-bellows/internal/control/events"
)

func TestBus_PublishFansOutToSubscribers(t *testing.T) {
	b := events.New()
	chA, cancelA := b.Subscribe()
	defer cancelA()
	chB, cancelB := b.Subscribe()
	defer cancelB()

	if got := b.Publish(events.Event{Type: "worker_provisioned"}); got != 2 {
		t.Fatalf("delivered: want 2 got %d", got)
	}

	select {
	case ev := <-chA:
		if ev.Type != "worker_provisioned" {
			t.Fatalf("A got %q", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("A did not receive")
	}
	select {
	case ev := <-chB:
		if ev.Type != "worker_provisioned" {
			t.Fatalf("B got %q", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("B did not receive")
	}
}

func TestBus_CancelStopsDelivery(t *testing.T) {
	b := events.New()
	ch, cancel := b.Subscribe()
	cancel()

	// Channel should be closed.
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
	if got := b.Subscribers(); got != 0 {
		t.Fatalf("Subscribers after cancel: want 0 got %d", got)
	}

	// Publish should be a no-op delivery-wise.
	if got := b.Publish(events.Event{Type: "noop"}); got != 0 {
		t.Fatalf("delivered after cancel: want 0 got %d", got)
	}
}

func TestBus_CancelIsIdempotent(_ *testing.T) {
	b := events.New()
	_, cancel := b.Subscribe()
	cancel()
	cancel() // second call must not panic on double-close
}

func TestBus_DropsSlowSubscribers(t *testing.T) {
	b := events.New()
	ch, _ := b.Subscribe() // never read this subscriber

	// Fill the buffer exactly; no drops yet.
	for range events.SubscriberBuffer {
		b.Publish(events.Event{Type: "flood"})
	}
	if got := b.Subscribers(); got != 1 {
		t.Fatalf("after buffer fill: want 1 subscriber got %d", got)
	}

	// One more publish — buffer is full, so the bus closes our channel and
	// drops us from the subscriber set.
	b.Publish(events.Event{Type: "overflow"})
	if got := b.Subscribers(); got != 0 {
		t.Fatalf("after overflow: want 0 subscribers got %d", got)
	}

	// Drain ch; values received first (they were buffered before close),
	// then receive eventually returns ok=false.
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
	if drained != events.SubscriberBuffer {
		t.Fatalf("drained: want %d got %d", events.SubscriberBuffer, drained)
	}
}

func TestBus_ConcurrentPublishAndSubscribe(_ *testing.T) {
	b := events.New()
	const subs = 20
	const eventsPerSub = 50

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
					if seen >= eventsPerSub {
						return
					}
				case <-deadline:
					return
				}
			}
		}()
	}
	// Give subscribers a moment to register before we start publishing.
	time.Sleep(20 * time.Millisecond)

	for range eventsPerSub * 2 {
		b.Publish(events.Event{Type: "tick"})
	}
	wg.Wait()
}
