// Package logbus is the in-process publish/subscribe bus the control plane
// uses to fan structured slog records out to StreamLogs subscribers, plus a
// bounded ring buffer of recent records so a new subscriber can replay
// history before live streaming begins.
//
// Each subscriber gets a bounded buffered channel; if a subscriber falls
// behind by more than the buffer, the bus closes that subscriber's channel
// (slow clients disconnect; the daemon never blocks). Producers call
// Publish from any goroutine; the bus is safe under concurrent fan-out.
//
// The companion Handler in handler.go wraps any slog.Handler so every record
// the daemon emits is also published to the bus — stderr text logging keeps
// working unchanged, and operators can `tail -f` over the control plane.
package logbus

import (
	"log/slog"
	"sync"
	"time"
)

// SubscriberBuffer is the per-subscriber channel capacity. Beyond this, the
// bus drops the subscriber rather than blocking the producer. Matches the
// events bus on purpose.
const SubscriberBuffer = 256

// HistoryCapacity is the size of the ring buffer of recent records the bus
// keeps so new subscribers can replay before live streaming begins.
const HistoryCapacity = 1000

// Record is one structured slog record the daemon emitted. Attrs is the
// flattened attribute map (see Handler for the flattening rules).
type Record struct {
	At      time.Time
	Level   slog.Level
	Message string
	Attrs   map[string]string
}

// Filter scopes a subscription or a history query to records whose attrs
// match. Empty fields mean "no filter on that dimension"; an empty Filter
// matches every record.
type Filter struct {
	InstanceID string
	Handle     string
}

// match returns true if r passes f.
func (f Filter) match(r Record) bool {
	if f.InstanceID != "" && r.Attrs["id"] != f.InstanceID {
		return false
	}
	if f.Handle != "" && r.Attrs["handle"] != f.Handle {
		return false
	}
	return true
}

type subscriber struct {
	ch     chan Record
	filter Filter
}

// Bus fans Records out to all current subscribers and keeps a ring buffer of
// the last HistoryCapacity records for replay. The zero value is unusable;
// call New.
type Bus struct {
	mu      sync.Mutex
	subs    map[*subscriber]struct{}
	history []Record // ring buffer, len up to HistoryCapacity
	next    int      // next write index into history
	full    bool     // true once we've wrapped once
}

// New returns an empty Bus ready for Subscribe/Publish.
func New() *Bus {
	return &Bus{
		subs:    map[*subscriber]struct{}{},
		history: make([]Record, HistoryCapacity),
	}
}

// Subscribe registers an unfiltered listener and returns its channel + a
// cancel func. The channel is buffered (SubscriberBuffer). Calling cancel
// closes the channel and removes it from the bus; safe to call multiple
// times. The cancel func is also what closes the channel when the bus drops
// the subscriber for slow consumption — receivers should range until close.
func (b *Bus) Subscribe() (<-chan Record, func()) {
	return b.subscribe(Filter{})
}

// SubscribeFiltered is like Subscribe but only delivers records that match
// the filter. Records that don't match are not counted against the
// subscriber's buffer.
func (b *Bus) SubscribeFiltered(filter Filter) (<-chan Record, func()) {
	return b.subscribe(filter)
}

func (b *Bus) subscribe(filter Filter) (<-chan Record, func()) {
	s := &subscriber{
		ch:     make(chan Record, SubscriberBuffer),
		filter: filter,
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subs[s]; ok {
				delete(b.subs, s)
				close(s.ch)
			}
			b.mu.Unlock()
		})
	}
	return s.ch, cancel
}

// Publish appends r to the history ring and sends it to every current
// subscriber whose filter matches. Subscribers whose buffer is full are
// dropped (channel closed, removed from the set) — the producer is never
// blocked by a slow consumer.
func (b *Bus) Publish(r Record) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Append to ring buffer.
	b.history[b.next] = r
	b.next++
	if b.next == HistoryCapacity {
		b.next = 0
		b.full = true
	}

	for s := range b.subs {
		if !s.filter.match(r) {
			continue
		}
		select {
		case s.ch <- r:
		default:
			delete(b.subs, s)
			close(s.ch)
		}
	}
}

// Subscribers returns the current subscriber count. Useful in tests and as a
// trivial /metrics gauge.
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// History returns up to n previously-buffered records that match filter, in
// chronological order (oldest first). If n is larger than the buffer's
// current contents, History returns everything it has. If n <= 0, History
// returns the empty slice.
func (b *Bus) History(n int, filter Filter) []Record {
	if n <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Walk history in chronological order. If full, oldest is at b.next;
	// otherwise oldest is at index 0 and length is b.next.
	var (
		start  int
		length int
	)
	if b.full {
		start = b.next
		length = HistoryCapacity
	} else {
		start = 0
		length = b.next
	}

	out := make([]Record, 0, min(n, length))
	for i := range length {
		idx := (start + i) % HistoryCapacity
		r := b.history[idx]
		if !filter.match(r) {
			continue
		}
		out = append(out, r)
	}
	// Trim to the most recent n records — operators want the tail, not
	// the head, of a long log history. Walk all matches then keep last n.
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
