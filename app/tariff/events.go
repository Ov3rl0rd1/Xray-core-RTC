package tariff

import (
	"sync"
	"sync/atomic"
	"time"
)

// eventBuffer is how many events a subscriber may fall behind by before it
// starts losing them. Generous enough to absorb a burst of reconnections,
// small enough that a subscriber which has stopped reading cannot pin an
// unbounded amount of memory.
const eventBuffer = 256

// bus fans events out to whoever is listening — in practice the management
// API's StreamEvents, so a panel can react to a user going over their quota
// instead of discovering it on the next poll.
//
// Publishing never blocks. A subscriber that stops reading loses events and is
// told how many when it comes back; the alternative, letting a wedged gRPC
// stream apply back-pressure to the data plane, would turn a monitoring
// problem into an outage.
type bus struct {
	mu   sync.RWMutex
	subs map[uint64]*subscriber
	next uint64
}

type subscriber struct {
	ch      chan *Event
	dropped atomic.Uint64
}

func newBus() *bus {
	return &bus{subs: make(map[uint64]*subscriber)}
}

// subscribe returns a channel of events and the id needed to stop it.
func (b *bus) subscribe() (uint64, *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	s := &subscriber{ch: make(chan *Event, eventBuffer)}
	b.subs[b.next] = s
	return b.next, s
}

// unsubscribe stops a subscription and closes its channel.
func (b *bus) unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(s.ch)
	}
}

// publish delivers an event to every subscriber that can take it.
func (b *bus) publish(e *Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		select {
		case s.ch <- e:
		default:
			s.dropped.Add(1)
		}
	}
}

// subscribers reports how many streams are listening. Used by the API's health
// output and by tests.
func (b *bus) subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// newEvent stamps an event with the given time.
func newEvent(now time.Time, kind EventKind, email string) *Event {
	return &Event{
		UnixNano: now.UnixNano(),
		Kind:     kind,
		Email:    email,
	}
}
