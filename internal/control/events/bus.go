// Package events is the in-process publish/subscribe bus the control plane
// uses to fan state-transition events out to StreamEvents subscribers (and,
// in PR5, to Prometheus counter aggregation).
//
// Each subscriber gets a bounded buffered channel; if a subscriber falls
// behind by more than the buffer, the bus closes that subscriber's channel
// (slow clients disconnect; the daemon never blocks). Producers call
// Publish from any goroutine; the bus is safe under concurrent fan-out.
package events

import (
	"sync"
	"time"
)

// SubscriberBuffer is the per-subscriber channel capacity. Beyond this, the
// bus drops the subscriber rather than blocking the producer.
const SubscriberBuffer = 256

// Event is one structured datum the orchestrator emits when a worker / job /
// reconcile transition fires. The Type is a stable wire-friendly slug
// (worker_provisioned, job_complete, reconcile_tick, ...); Attrs holds the
// per-event detail (instance id, ip, handle, counts, ...).
type Event struct {
	At    time.Time
	Type  string
	Attrs map[string]string
}

// Bus fans events out to all current subscribers. The zero value is unusable;
// call New.
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// New returns an empty Bus ready for Subscribe/Publish.
func New() *Bus {
	return &Bus{subs: map[chan Event]struct{}{}}
}

// Subscribe registers a new listener and returns its event channel + a
// cancel func. The channel is buffered (SubscriberBuffer). Calling cancel
// closes the channel and removes it from the bus; safe to call multiple
// times. The cancel func is also what closes the channel when the bus drops
// the subscriber for slow consumption — receivers should range until close.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, SubscriberBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subs[ch]; ok {
				delete(b.subs, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}
	return ch, cancel
}

// Publish sends ev to every current subscriber. Subscribers whose buffer is
// full are dropped (channel closed, removed from the set) — the producer is
// never blocked by a slow consumer. Returns the number of subscribers that
// received the event.
func (b *Bus) Publish(ev Event) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	delivered := 0
	for ch := range b.subs {
		select {
		case ch <- ev:
			delivered++
		default:
			delete(b.subs, ch)
			close(ch)
		}
	}
	return delivered
}

// Subscribers returns the current subscriber count. Useful in tests and as a
// trivial /metrics gauge.
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
