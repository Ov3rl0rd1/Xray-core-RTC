package shaper

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock drives the parts of the shaper that reason about elapsed time —
// credit refill, device activity, eviction — without making the tests wait for
// any of it. It is deliberately not used for the throughput tests: those
// exercise real sleeping, and a clock the sleeps do not follow would make them
// meaningless.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const (
	kb = 1024
	mb = 1024 * kb
)

// --- credit bucket ---------------------------------------------------------

func TestCreditBucketStartsFullAndDrains(t *testing.T) {
	clk := newFakeClock()
	b := newCreditBucket(1000, time.Hour, clk.Now())

	if got := b.takeUpTo(clk.Now(), 400); got != 400 {
		t.Fatalf("first take = %d, want 400", got)
	}
	if got := b.takeUpTo(clk.Now(), 900); got != 600 {
		t.Fatalf("over-take = %d, want the remaining 600", got)
	}
	if got := b.takeUpTo(clk.Now(), 1); got != 0 {
		t.Fatalf("take from empty = %d, want 0", got)
	}
}

func TestCreditBucketRefillsOverItsPeriod(t *testing.T) {
	clk := newFakeClock()
	b := newCreditBucket(1000, time.Hour, clk.Now())
	b.takeUpTo(clk.Now(), 1000)

	clk.Advance(30 * time.Minute)
	if got := b.level(clk.Now()); got < 480 || got > 520 {
		t.Fatalf("level after half a refill period = %d, want ~500", got)
	}

	clk.Advance(time.Hour) // well past full
	if got := b.level(clk.Now()); got != 1000 {
		t.Fatalf("level after a full period = %d, want the capacity 1000", got)
	}
	if !b.full(clk.Now()) {
		t.Fatal("bucket should report full")
	}
}

func TestCreditBucketRefundIsCappedAtCapacity(t *testing.T) {
	clk := newFakeClock()
	b := newCreditBucket(1000, time.Hour, clk.Now())
	b.takeUpTo(clk.Now(), 300)
	b.refund(1000)

	if got := b.level(clk.Now()); got != 1000 {
		t.Fatalf("level after over-refund = %d, want the capacity 1000", got)
	}
}

func TestCreditBucketResizeKeepsAccumulatedCredit(t *testing.T) {
	clk := newFakeClock()
	b := newCreditBucket(1000, time.Hour, clk.Now())
	b.takeUpTo(clk.Now(), 700) // 300 left

	// Moving to a bigger plan must not wipe what is left...
	b.resize(clk.Now(), 5000, time.Hour)
	if got := b.level(clk.Now()); got != 300 {
		t.Fatalf("level after growing the budget = %d, want the 300 carried over", got)
	}
	// ...and moving to a smaller one must not hand out a fresh full budget.
	b.resize(clk.Now(), 100, time.Hour)
	if got := b.level(clk.Now()); got != 100 {
		t.Fatalf("level after shrinking the budget = %d, want the new capacity 100", got)
	}
}

func TestCreditBucketIgnoresBackwardsClock(t *testing.T) {
	clk := newFakeClock()
	b := newCreditBucket(1000, time.Hour, clk.Now())
	b.takeUpTo(clk.Now(), 1000)

	clk.Advance(-time.Hour) // suspend/resume, NTP step
	if got := b.level(clk.Now()); got != 0 {
		t.Fatalf("a clock that went backwards minted %d credit, want 0", got)
	}
}

// --- sizing invariants -----------------------------------------------------

// TestChunkAlwaysFitsInBurst guards the property the whole reservation path
// relies on: a chunk reservation can never be refused for exceeding the
// bucket. If this breaks, waitFor silently stops shaping.
func TestChunkAlwaysFitsInBurst(t *testing.T) {
	rates := []int64{
		1, 1000, 8 * kb, 64 * kb, 128 * kb, mb, 10 * mb, 100 * mb, 1000 * mb,
	}
	for _, bps := range rates {
		chunk := chunkFor(bps)
		if chunk < minChunk || chunk > maxChunk {
			t.Fatalf("bps=%d: chunk %d outside [%d,%d]", bps, chunk, minChunk, maxChunk)
		}
		if burst := burstFor(bps, chunk); burst < chunk {
			t.Fatalf("bps=%d: burst %d smaller than chunk %d", bps, burst, chunk)
		}
		// The device share can be an arbitrary fraction of the user's rate,
		// and its burst is computed against the user's chunk.
		for _, div := range []int64{2, 8, 64, 1024} {
			if burst := burstFor(bps/div, chunk); burst < chunk {
				t.Fatalf("bps=%d share=%d: burst %d smaller than chunk %d",
					bps, bps/div, burst, chunk)
			}
		}
	}
}

// --- throughput ------------------------------------------------------------

// steadyLimits returns a policy with only the aggregate cap active, so a test
// measures the tariff rather than the boost or the priority allowance.
func steadyLimits(bps int64) Limits {
	return Limits{
		UplinkBPS:     bps,
		DownlinkBPS:   bps,
		PriorityShare: -1, // disabled
	}
}

func attach(t *testing.T, l Limits, device string) *Flow {
	t.Helper()
	r := New(Config{Resolve: func(User) Limits { return l }})
	f := r.Attach(User{Email: "u@example"}, device, Down)
	if f == nil {
		t.Fatal("Attach returned nil for a limited user")
	}
	t.Cleanup(f.Release)
	return f
}

func TestWaitEnforcesTheAggregateRate(t *testing.T) {
	const bps = 512 * kb
	f := attach(t, steadyLimits(bps), "dev")
	ctx := context.Background()

	// Spend the initial burst first; it is a head start by design, not part
	// of the steady rate we are measuring.
	if err := f.Wait(ctx, burstFor(bps, chunkFor(bps))); err != nil {
		t.Fatal(err)
	}

	const payload = 256 * kb
	start := time.Now()
	if err := f.Wait(ctx, payload); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	want := time.Duration(float64(payload) / float64(bps) * float64(time.Second))
	if elapsed < want*7/10 || elapsed > want*2 {
		t.Fatalf("moving %d B at %d B/s took %v, want about %v", payload, bps, elapsed, want)
	}
}

// TestWaitChunksLargeWrites checks that a write far larger than the bucket is
// paced rather than rejected. The pre-fork limiter failed this outright:
// rate.Limiter.WaitN returns an error when n exceeds the burst, which killed
// the connection instead of slowing it.
func TestWaitChunksLargeWrites(t *testing.T) {
	const bps = 4 * mb
	f := attach(t, steadyLimits(bps), "dev")

	// Twice this tariff's burst, so the write cannot be satisfied outright.
	payload := 2 * burstFor(bps, chunkFor(bps))
	if err := f.Wait(context.Background(), payload); err != nil {
		t.Fatalf("a write larger than the bucket failed instead of being paced: %v", err)
	}
}

func TestWaitIsFreeForAnUnlimitedDirection(t *testing.T) {
	// Uplink capped, downlink not: the uncapped direction must cost nothing.
	r := New(Config{Resolve: func(User) Limits {
		return Limits{UplinkBPS: 1 * kb}
	}})
	f := r.Attach(User{Email: "u@example"}, "dev", Down)
	defer f.Release()

	start := time.Now()
	for i := 0; i < 100; i++ {
		if err := f.Wait(context.Background(), 1*mb); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("an unlimited direction blocked for %v", elapsed)
	}
}

func TestWaitReturnsWhenTheContextEnds(t *testing.T) {
	f := attach(t, steadyLimits(4*kb), "dev")
	ctx, cancel := context.WithCancel(context.Background())

	// Drain the bucket so the next wait is a long one.
	if err := f.Wait(context.Background(), burstFor(4*kb, chunkFor(4*kb))); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := f.Wait(ctx, 64*kb) // would take ~16 s at this rate
	if err == nil {
		t.Fatal("Wait returned nil after its context was cancelled")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Wait took %v to notice cancellation", elapsed)
	}
}

// --- speed boost -----------------------------------------------------------

func TestBoostRunsAheadOfTheTariffThenSettlesOntoIt(t *testing.T) {
	limits := Limits{
		// A tariff far too slow to explain the first megabyte.
		UplinkBPS:     8 * kb,
		DownlinkBPS:   8 * kb,
		BoostBytes:    1 * mb,
		BoostRefill:   time.Hour,
		PriorityShare: -1,
	}
	f := attach(t, limits, "dev")
	ctx := context.Background()

	start := time.Now()
	if err := f.Wait(ctx, 1*mb); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("the boost budget took %v to spend, want it to be immediate", elapsed)
	}

	// With the budget gone, the tariff must reassert itself. The bucket has
	// been idle and holds a burst, so drain that first.
	if err := f.Wait(ctx, burstFor(8*kb, chunkFor(8*kb))); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if err := f.Wait(ctx, 4*kb); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("after the boost was spent a 4 KiB write took %v at 8 KiB/s, want ~500ms", elapsed)
	}
}

// TestBoostIsNotDebitedAgainstTheTariff pins down the difference between the
// boost and the priority allowance: spending the boost must not leave the user
// slower afterwards than the plan they paid for.
func TestBoostIsNotDebitedAgainstTheTariff(t *testing.T) {
	clk := newFakeClock()
	limits := Limits{
		DownlinkBPS:   64 * kb,
		BoostBytes:    1 * mb,
		BoostRefill:   time.Hour,
		PriorityShare: -1,
	}
	r := New(Config{Resolve: func(User) Limits { return limits }, Now: clk.Now})
	f := r.Attach(User{Email: "u@example"}, "dev", Down)
	defer f.Release()

	st := f.s.st.Load().dir(Down)
	before := st.steady.TokensAt(clk.Now())
	if err := f.Wait(context.Background(), 1*mb); err != nil {
		t.Fatal(err)
	}
	after := st.steady.TokensAt(clk.Now())

	if after < before-1 {
		t.Fatalf("the tariff bucket lost %.0f tokens to the boost; the boost must be free",
			before-after)
	}
}

// --- priority allowance ----------------------------------------------------

// TestShortFlowSkipsTheQueueOfABulkTransfer is the behaviour the whole
// chunk-plus-allowance design exists for: on a slow plan being saturated by a
// download, a DNS lookup or a TLS handshake must still complete promptly.
func TestShortFlowSkipsTheQueueOfABulkTransfer(t *testing.T) {
	const bps = 64 * kb
	limits := Limits{DownlinkBPS: bps, UplinkBPS: bps}
	r := New(Config{Resolve: func(User) Limits { return limits }})
	u := User{Email: "u@example"}

	bulk := r.Attach(u, "dev", Down)
	defer bulk.Release()

	// Saturate the plan from one flow, and keep it saturated. Cancel before
	// waiting: the goroutine's loop condition is the context, so the other
	// order would wait forever.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			// Writes this large are bulk by definition and must not be able to
			// draw on the shared priority allowance.
			if err := bulk.Wait(ctx, 32*kb); err != nil {
				return
			}
		}
	}()

	// Let the bulk flow build a real backlog before measuring.
	time.Sleep(150 * time.Millisecond)

	short := r.Attach(u, "dev", Down)
	defer short.Release()
	start := time.Now()
	if err := short.Wait(ctx, 4*kb); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	// Without the allowance this write queues behind the bulk flow's backlog
	// and takes the better part of a second.
	if elapsed > 150*time.Millisecond {
		t.Fatalf("a 4 KiB short write waited %v behind a saturating transfer", elapsed)
	}
}

// TestPriorityAllowanceIsBorrowedNotFree is the other half of the contract:
// short flows may jump the queue, but the bytes are still charged, so a user
// cannot beat their cap by keeping every flow short.
func TestPriorityAllowanceIsBorrowedNotFree(t *testing.T) {
	clk := newFakeClock()
	limits := Limits{DownlinkBPS: 64 * kb}
	r := New(Config{Resolve: func(User) Limits { return limits }, Now: clk.Now})
	f := r.Attach(User{Email: "u@example"}, "dev", Down)
	defer f.Release()

	st := f.s.st.Load().dir(Down)
	before := st.steady.TokensAt(clk.Now())
	const payload = 8 * kb
	if err := f.Wait(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	after := st.steady.TokensAt(clk.Now())

	if spent := before - after; spent < payload-1 {
		t.Fatalf("the tariff bucket was charged %.0f of %d bytes; the allowance must be borrowed",
			spent, payload)
	}
}

// --- fairness between devices ---------------------------------------------

func TestDevicesShareTheCapRoughlyEvenly(t *testing.T) {
	const bps = 512 * kb
	limits := Limits{DownlinkBPS: bps, PriorityShare: -1}
	r := New(Config{Resolve: func(User) Limits { return limits }})
	u := User{Email: "shared@example"}

	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()

	var moved [2]atomic.Int64
	var wg sync.WaitGroup
	for i, dev := range []string{"10.0.0.1", "10.0.0.2"} {
		f := r.Attach(u, dev, Down)
		wg.Add(1)
		go func(i int, f *Flow) {
			defer wg.Done()
			defer f.Release()
			for ctx.Err() == nil {
				if err := f.Wait(ctx, 8*kb); err != nil {
					return
				}
				moved[i].Add(8 * kb)
			}
		}(i, f)
	}
	wg.Wait()

	a, b := moved[0].Load(), moved[1].Load()
	if a == 0 || b == 0 {
		t.Fatalf("one device was starved entirely: %d vs %d", a, b)
	}
	ratio := float64(a) / float64(b)
	if ratio < 0.55 || ratio > 1.8 {
		t.Fatalf("devices moved %d and %d bytes (ratio %.2f), want a roughly even split", a, b, ratio)
	}
}

func TestIdleDeviceDoesNotHoldOntoItsShare(t *testing.T) {
	clk := newFakeClock()
	limits := Limits{DownlinkBPS: 1 * mb, Overcommit: 1.5}
	r := New(Config{Resolve: func(User) Limits { return limits }, Now: clk.Now})
	u := User{Email: "u@example"}

	busy := r.Attach(u, "busy", Down)
	defer busy.Release()
	idle := r.Attach(u, "idle", Down)
	defer idle.Release()

	// Both have just attached, so both count as active: 1.5/2 of the cap each.
	shareWith := func() float64 {
		return float64(busy.dev.limiter(Down).Limit())
	}
	if got, want := shareWith(), 1.5*mb/2; got < want*0.95 || got > want*1.05 {
		t.Fatalf("share with two active devices = %.0f B/s, want ~%.0f", got, want)
	}

	// Let the idle one fall outside the activity window and keep the busy one
	// inside it, then force a recount.
	clk.Advance(activeWindow + time.Second)
	busy.dev.lastActive.Store(clk.Now().UnixNano())
	busy.s.maybeRebalance(clk.Now())

	// One active device: the overcommit would give it 1.5x the cap, which is
	// clamped to the cap itself.
	if got := shareWith(); got < 1*mb*0.95 {
		t.Fatalf("share once its neighbour went idle = %.0f B/s, want the full %d", got, 1*mb)
	}
}

// --- policy changes and lifecycle -----------------------------------------

func TestSetLimitsAppliesToLiveFlows(t *testing.T) {
	clk := newFakeClock()
	r := New(Config{Resolve: func(User) Limits {
		return Limits{DownlinkBPS: 1 * mb}
	}, Now: clk.Now})
	f := r.Attach(User{Email: "u@example"}, "dev", Down)
	defer f.Release()

	f.s.SetLimits(Limits{DownlinkBPS: 64 * kb})

	if got := f.s.st.Load().dir(Down).bps; got != 64*kb {
		t.Fatalf("live limits after SetLimits = %d B/s, want %d", got, 64*kb)
	}
	if got := float64(f.dev.limiter(Down).Limit()); got > 64*kb {
		t.Fatalf("the device limiter kept the old cap: %.0f B/s", got)
	}
}

func TestSetLimitsCarriesTheBoostBudgetOver(t *testing.T) {
	clk := newFakeClock()
	limits := Limits{DownlinkBPS: 64 * kb, BoostBytes: 1000, BoostRefill: time.Hour}
	r := New(Config{Resolve: func(User) Limits { return limits }, Now: clk.Now})
	f := r.Attach(User{Email: "u@example"}, "dev", Down)
	defer f.Release()

	f.s.st.Load().dir(Down).boost.takeUpTo(clk.Now(), 900) // 100 left

	limits.DownlinkBPS = 128 * kb
	f.s.SetLimits(limits)

	if got := f.s.st.Load().dir(Down).boost.level(clk.Now()); got != 100 {
		t.Fatalf("boost budget after a tariff change = %d, want the 100 carried over", got)
	}
}

func TestIdleUsersAreEvictedOnlyOnceTheirCreditHasRecovered(t *testing.T) {
	clk := newFakeClock()
	limits := Limits{DownlinkBPS: 64 * kb, BoostBytes: 1 * mb, BoostRefill: time.Hour}
	r := New(Config{Resolve: func(User) Limits { return limits }, Now: clk.Now})

	f := r.Attach(User{Email: "spender@example"}, "dev", Down)
	f.s.st.Load().dir(Down).boost.takeUpTo(clk.Now(), 1*mb) // spend it all
	f.Release()

	// Long enough to be idle, but not long enough to have earned the boost
	// back — forgetting the user here would hand them a fresh budget on
	// reconnect, which is exactly the abuse the budget exists to prevent.
	clk.Advance(minIdleEvict + time.Minute)
	r.Attach(User{Email: "other@example"}, "dev", Down).Release()
	if got := r.Len(); got != 2 {
		t.Fatalf("users tracked while credit was still recovering = %d, want 2", got)
	}

	clk.Advance(time.Hour)
	r.Attach(User{Email: "third@example"}, "dev", Down).Release()
	if r.Lookup("spender@example") != nil {
		t.Fatal("a user idle past their refill period should have been swept")
	}
}

func TestAttachedUsersAreNeverEvicted(t *testing.T) {
	clk := newFakeClock()
	r := New(Config{Resolve: func(User) Limits {
		return Limits{DownlinkBPS: 64 * kb}
	}, Now: clk.Now})

	held := r.Attach(User{Email: "held@example"}, "dev", Down)
	defer held.Release()

	clk.Advance(24 * time.Hour)
	r.Attach(User{Email: "other@example"}, "dev", Down).Release()

	if r.Lookup("held@example") == nil {
		t.Fatal("a user with a live connection was swept")
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	clk := newFakeClock()
	r := New(Config{Resolve: func(User) Limits {
		return Limits{DownlinkBPS: 64 * kb}
	}, Now: clk.Now})
	u := User{Email: "u@example"}

	a := r.Attach(u, "dev", Down)
	b := r.Attach(u, "dev", Up)

	a.Release()
	a.Release()
	a.Release()

	s := r.Lookup(u.Email)
	if s == nil {
		t.Fatal("shaper vanished")
	}
	s.mu.Lock()
	attached, devices := s.attached, len(s.devices)
	s.mu.Unlock()
	if attached != 1 || devices != 1 {
		t.Fatalf("repeated Release corrupted the refcount: attached=%d devices=%d", attached, devices)
	}

	b.Release()
	s.mu.Lock()
	attached, devices = s.attached, len(s.devices)
	s.mu.Unlock()
	if attached != 0 || devices != 0 {
		t.Fatalf("after the last Release: attached=%d devices=%d, want 0 and 0", attached, devices)
	}
}

func TestNilFlowIsUsable(t *testing.T) {
	var f *Flow
	if err := f.Wait(context.Background(), 4096); err != nil {
		t.Fatalf("nil flow Wait = %v, want nil", err)
	}
	f.Release() // must not panic
}

func TestAnonymousConnectionsAreNotShaped(t *testing.T) {
	r := New(Config{Resolve: func(User) Limits {
		return Limits{DownlinkBPS: 1}
	}})
	if f := r.Attach(User{}, "dev", Down); f != nil {
		t.Fatal("a connection with no user should not be shaped")
	}
}

func TestUnlimitedReportsOnlyWhenBothDirectionsAreOpen(t *testing.T) {
	cases := []struct {
		name string
		l    Limits
		want bool
	}{
		{"zero value", Limits{}, true},
		{"uplink only", Limits{UplinkBPS: 1}, false},
		{"downlink only", Limits{DownlinkBPS: 1}, false},
		{"both", Limits{UplinkBPS: 1, DownlinkBPS: 1}, false},
	}
	for _, c := range cases {
		if got := c.l.Unlimited(); got != c.want {
			t.Errorf("%s: Unlimited() = %v, want %v", c.name, got, c.want)
		}
	}
}
