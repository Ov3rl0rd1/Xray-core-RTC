package olcrtc

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// clock drives the pool's cooldowns without the test waiting for them.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testPool(t *testing.T, primary string, fallbacks ...string) (*roomPool, *clock) {
	t.Helper()
	c := newClock()
	p := newRoomPool(primary, fallbacks, time.Minute)
	p.now = c.Now
	return p, c
}

var errDead = errors.New("provider refused the room")

func TestPoolPrefersThePrimary(t *testing.T) {
	p, _ := testPool(t, "primary", "backup")

	for i := 0; i < 3; i++ {
		room, switched := p.next()
		if room != "primary" {
			t.Fatalf("attempt %d chose %q, want the primary", i, room)
		}
		if i > 0 && switched {
			t.Fatalf("attempt %d reported a switch without one", i)
		}
		p.succeeded(room)
	}
}

func TestPoolMovesOnAfterAFailure(t *testing.T) {
	p, _ := testPool(t, "primary", "backup")

	room, _ := p.next()
	p.failed(room, time.Second, errDead)

	next, switched := p.next()
	if next != "backup" {
		t.Fatalf("after the primary failed the pool chose %q, want the backup", next)
	}
	if !switched {
		t.Fatal("moving to another room was not reported as a switch")
	}
}

// TestPoolReturnsToThePrimary is why the cooldown lengthens rather than the
// pool simply advancing: the fallbacks are there to keep the service up, not
// to become the new normal.
func TestPoolReturnsToThePrimary(t *testing.T) {
	p, c := testPool(t, "primary", "backup")

	room, _ := p.next()
	p.failed(room, time.Second, errDead)
	if next, _ := p.next(); next != "backup" {
		t.Fatalf("chose %q, want the backup", next)
	}

	c.Advance(2 * time.Minute) // the primary's cooldown lapses
	next, switched := p.next()
	if next != "primary" {
		t.Fatalf("chose %q once the primary was free again, want the primary", next)
	}
	if !switched {
		t.Fatal("returning to the primary was not reported as a switch")
	}
}

func TestCooldownLengthensWithEachFailure(t *testing.T) {
	p, c := testPool(t, "only")

	var waits []time.Duration
	for i := 0; i < 4; i++ {
		room, _ := p.next()
		p.failed(room, time.Second, errDead)
		state := p.snapshot()[0]
		waits = append(waits, state.cooldownUntil.Sub(c.Now()))
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] <= waits[i-1] {
			t.Fatalf("cooldowns did not lengthen: %v", waits)
		}
	}
	if waits[0] != time.Minute {
		t.Fatalf("first cooldown = %v, want the configured minute", waits[0])
	}
}

// TestALongRunIsNotTheRoomsFault guards against rotating away from a room that
// works: a carrier that stayed up for hours and then ended says nothing bad
// about the room it was in.
func TestALongRunIsNotTheRoomsFault(t *testing.T) {
	p, c := testPool(t, "primary", "backup")

	room, _ := p.next()
	p.failed(room, time.Hour, errDead)

	if state := p.snapshot()[0]; state.failures != 0 {
		t.Fatalf("a run of an hour counted %d failures against the room", state.failures)
	}
	if next, _ := p.next(); next != "primary" {
		t.Fatalf("chose %q at %v, want to stay on the primary", next, c.Now())
	}
}

// TestEverythingInCooldownStillPicksSomething: a tunnel that refuses to run
// because every room is resting is worse than one that retries too eagerly.
func TestEverythingInCooldownStillPicksSomething(t *testing.T) {
	p, _ := testPool(t, "a", "b")

	for i := 0; i < 4; i++ {
		room, _ := p.next()
		p.failed(room, time.Second, errDead)
	}
	if room, _ := p.next(); room == "" {
		t.Fatal("the pool gave up when every room was in cooldown")
	}
}

func TestSuccessClearsTheHistory(t *testing.T) {
	p, _ := testPool(t, "only")

	room, _ := p.next()
	p.failed(room, time.Second, errDead)
	p.succeeded(room)

	state := p.snapshot()[0]
	if state.failures != 0 || !state.cooldownUntil.IsZero() || !state.up {
		t.Fatalf("a room that worked again is still marked bad: %+v", state)
	}
	if next, _ := p.next(); next != "only" {
		t.Fatalf("chose %q, want the room that just worked", next)
	}
}

func TestPoolIgnoresBlanksAndDuplicates(t *testing.T) {
	p := newRoomPool(" primary ", []string{"", "primary", "backup", "backup", "   "}, 0)
	if got := p.Len(); got != 2 {
		t.Fatalf("pool holds %d rooms, want 2 (the primary and one backup)", got)
	}
	if room, _ := p.next(); room != "primary" {
		t.Fatalf("chose %q, want the trimmed primary", room)
	}
}

func TestEmptyPoolIsHarmless(t *testing.T) {
	p := newRoomPool("", nil, 0)
	room, switched := p.next()
	if room != "" || switched {
		t.Fatalf("an empty pool returned %q / %v", room, switched)
	}
	// Neither of these has anything to record, and neither may panic.
	p.succeeded("anything")
	p.failed("anything", time.Second, errDead)
	if p.Len() != 0 {
		t.Fatal("an empty pool grew a room")
	}
}

func TestSingleRoomNeverReportsASwitch(t *testing.T) {
	p, c := testPool(t, "only")

	for i := 0; i < 3; i++ {
		room, switched := p.next()
		if room != "only" {
			t.Fatalf("chose %q", room)
		}
		if switched {
			t.Fatal("a pool with one room reported switching to it")
		}
		p.failed(room, time.Second, errDead)
		c.Advance(10 * time.Minute)
	}
}
