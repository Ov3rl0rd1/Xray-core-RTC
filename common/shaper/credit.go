package shaper

import (
	"sync"
	"time"
)

// creditBucket is a token bucket whose tokens are never waited for: takeUpTo
// hands back however much credit happens to be available right now, and the
// caller pays for the remainder some other way. It exists because two of this
// package's behaviours are budgets rather than rates:
//
//   - the speed boost, which lets a session run at line rate until its budget
//     is spent and only then settles onto the tariff;
//   - the priority allowance, which lets a short flow (a DNS lookup, a TLS
//     handshake, a chat message) skip the queue a bulk transfer has built up.
//
// Tokens are int64 bytes rather than the float64 that golang.org/x/time/rate
// uses, because a boost budget is measured in hundreds of megabytes while
// rate.Limiter's burst parameter is an int — which is 32 bits wide on the
// 32-bit ARM builds this fork ships.
type creditBucket struct {
	mu       sync.Mutex
	tokens   int64
	capacity int64
	perSec   float64
	last     time.Time
}

// newCreditBucket returns a bucket that starts full. Starting full is
// deliberate: a user who has just been created, or who has been idle long
// enough to be evicted and recreated, should get their boost immediately —
// anything else would punish exactly the people who use the service least.
//
// refill is the time an empty bucket takes to reach capacity again. A
// non-positive refill makes the bucket one-shot: once spent, it stays empty
// for the lifetime of the bucket.
func newCreditBucket(capacity int64, refill time.Duration, now time.Time) *creditBucket {
	b := &creditBucket{capacity: capacity, tokens: capacity, last: now}
	b.setRefill(refill)
	return b
}

func (b *creditBucket) setRefill(refill time.Duration) {
	if refill > 0 && b.capacity > 0 {
		b.perSec = float64(b.capacity) / refill.Seconds()
	} else {
		b.perSec = 0
	}
}

// takeUpTo spends at most n bytes of credit and reports how much it spent,
// which may be zero. It never blocks and never fails.
func (b *creditBucket) takeUpTo(now time.Time, n int64) int64 {
	if n <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refillLocked(now)
	if b.tokens <= 0 {
		return 0
	}
	if n > b.tokens {
		n = b.tokens
	}
	b.tokens -= n
	return n
}

// refund puts credit back that was spent on something which then did not
// happen, so that an interrupted write does not quietly consume a user's
// budget. Credit above the current capacity is dropped.
func (b *creditBucket) refund(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.tokens += n
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
}

// resize changes the bucket's capacity and refill period in place, keeping the
// credit already accumulated (clamped to the new capacity). Used when a user's
// tariff changes under a live connection: dropping to a smaller plan must not
// hand the user a fresh full budget, and moving up to a larger one must not
// wipe the budget they had.
func (b *creditBucket) resize(now time.Time, capacity int64, refill time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refillLocked(now)
	b.capacity = capacity
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.setRefill(refill)
}

// level reports the credit currently available. Only used by tests and by the
// state snapshot the management API serves.
func (b *creditBucket) level(now time.Time) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refillLocked(now)
	return b.tokens
}

// full reports whether the bucket has recovered its whole budget. The registry
// uses it to decide when an idle user can be forgotten: once every bucket is
// full, keeping the user in the map and recreating them on their next
// connection are indistinguishable.
func (b *creditBucket) full(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refillLocked(now)
	return b.tokens >= b.capacity
}

func (b *creditBucket) refillLocked(now time.Time) {
	if !now.After(b.last) {
		// A clock that went backwards (suspend/resume, NTP step) must not
		// mint credit; just re-anchor and carry on.
		b.last = now
		return
	}
	elapsed := now.Sub(b.last)
	b.last = now
	if b.perSec <= 0 {
		return
	}
	b.tokens += int64(elapsed.Seconds() * b.perSec)
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
}
