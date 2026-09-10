// Package shaper enforces a per-user traffic policy: an aggregate cap the
// user's devices share fairly, a speed boost that makes the first stretch of a
// session feel unmetered, and a priority allowance that keeps short flows
// responsive while a bulk transfer is saturating the cap.
//
// It is deliberately a shaper and not a congestion control algorithm. BBR and
// Brutal answer "how fast may I send before I hurt the path"; they live in the
// sender's TCP/QUIC stack, they know nothing about users, and Brutal in
// particular needs the peer's cooperation — which rules it out here, since the
// clients are stock Xray builds this fork does not control. Deciding how much
// of the server's capacity a given subscription gets, and how that is divided
// between the people sharing it, is a scheduling question, and this is where
// it is answered.
//
// # Where it sits
//
// One Registry per Xray instance holds one [Shaper] per user, and each shaper
// holds one bucket per direction plus one limiter per device. Every connection
// takes a [Flow] from it and calls [Flow.Wait] before writing. Because the
// hook lives at the dispatcher — the single point every proxied connection
// crosses — the policy applies uniformly across VLESS, Hysteria, olcRTC and
// anything else, and across all of a user's devices at once.
//
// # Why the queue is chunked
//
// The obvious implementation, one token bucket per user with a reservation for
// each write, has a failure mode that is much worse than it looks: a download
// writing 512 KiB reserves 512 KiB of tokens up front, and every other flow of
// that user — a DNS lookup, a TLS handshake, a keystroke in an SSH session —
// queues behind the whole of it. On a 1 Mbit/s tariff that is four seconds of
// added latency, and it is what makes a rate-limited VPN feel broken rather
// than slow. Wait therefore claims tokens in chunks of roughly 20 ms of the
// user's rate, so a flow only ever waits behind one chunk per competitor, and
// short flows can additionally skip the queue on borrowed credit.
package shaper

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Tuning constants for the reservation queue. These are properties of the
// algorithm rather than of any tariff, so they are not exposed on Limits.
const (
	// chunkDivisor sets the chunk to 1/50th of a second of the user's rate.
	// Queueing delay for a flow is about one chunk per competing flow, so 20 ms
	// keeps a user with a dozen busy connections inside a quarter second even
	// on a slow plan.
	chunkDivisor = 50

	// minChunk keeps very slow tariffs from paying a reservation per packet.
	minChunk = 2 * 1024
	// maxChunk keeps fast tariffs from rebuilding the latency problem the
	// chunking exists to solve.
	maxChunk = 64 * 1024

	// maxBurst caps how much traffic may arrive in one instant after an idle
	// period, regardless of how fast the tariff is.
	maxBurst = 2 * 1024 * 1024

	// activeWindow is how long a device counts as active after its last byte,
	// for the purpose of dividing the cap between devices.
	activeWindow = 5 * time.Second

	// rebalanceInterval is the shortest gap between two recalculations of the
	// per-device shares. Membership changes rebalance immediately regardless.
	rebalanceInterval = time.Second

	// maxPriorityWrite is the largest write that may draw on the priority
	// allowance.
	//
	// The allowance is shared by all of a user's flows, so eligibility has to
	// be something a bulk transfer fails from its very first write — the
	// cumulative test alone is not enough, because a download would drain the
	// whole shared allowance while spending its own quota of short-flow bytes.
	// Write size is the signal that separates them: a DNS query, a TLS
	// handshake or an API call arrives in a few kilobytes, while a transfer
	// running at any speed worth prioritising against hands over buffers far
	// larger than this.
	maxPriorityWrite = 16 * 1024

	// minIdleEvict is the shortest a user is remembered after their last
	// connection closes. See Registry.
	minIdleEvict = 5 * time.Minute

	// defaultSweep is how often an idle user sweep runs.
	defaultSweep = time.Minute
)

// Direction distinguishes the two independently shaped halves of a connection.
type Direction int

const (
	// Up is client to internet.
	Up Direction = iota
	// Down is internet to client.
	Down
)

// String implements fmt.Stringer.
func (d Direction) String() string {
	if d == Up {
		return "uplink"
	}
	return "downlink"
}

// User identifies whose traffic is being shaped. Email is the key — it is what
// every Xray protocol agrees on and what the management API addresses users
// by — while Level is carried along so a resolver can fall back to
// level-based defaults for a user nobody has assigned a tariff to.
type User struct {
	Email string
	Level uint32
}

// Config configures a [Registry].
type Config struct {
	// Resolve returns the limits for a user. It is called when a shaper is
	// first created for that user and again whenever [Registry.Invalidate] is
	// called for them, so it is the seam through which a management API
	// changes tariffs at runtime. A nil Resolve shapes nobody.
	Resolve func(User) Limits

	// Now replaces time.Now. Tests set it; production leaves it nil.
	Now func() time.Time

	// Sweep is how often idle users are swept out of the map. Zero uses
	// defaultSweep.
	Sweep time.Duration
}

// Registry owns the live shaper for every user.
//
// Shapers outlive the connections that created them, because the credit they
// hold is the whole anti-abuse story: if disconnecting forgot a user, a client
// could reconnect to refill its speed boost, and the boost would be worth
// exactly nothing. A user is therefore only forgotten once they have had no
// connection for at least minIdleEvict *and* every credit bucket they own has
// refilled — at which point remembering them and recreating them are
// indistinguishable, so forgetting them is free.
//
// The sweep runs opportunistically inside Attach rather than on a goroutine,
// so a Registry needs no lifecycle management and leaks nothing if it is
// dropped.
type Registry struct {
	resolve func(User) Limits
	now     func() time.Time
	sweep   time.Duration

	mu        sync.Mutex
	users     map[string]*Shaper
	nextSweep time.Time
}

// New returns a Registry.
func New(cfg Config) *Registry {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	sweep := cfg.Sweep
	if sweep <= 0 {
		sweep = defaultSweep
	}
	return &Registry{
		resolve:   cfg.Resolve,
		now:       now,
		sweep:     sweep,
		users:     make(map[string]*Shaper),
		nextSweep: now().Add(sweep),
	}
}

// Attach registers one direction of one connection with the user's shaper and
// returns the [Flow] it must call Wait on. device is an opaque key that groups
// connections coming from the same place — the source IP for socket inbounds,
// a real device identifier where the protocol has one — and is what the fair
// share between the people sharing a subscription is computed over.
//
// The returned Flow must be released exactly once, with [Flow.Release]. A nil
// Flow is returned for an anonymous connection and is safe to use.
func (r *Registry) Attach(u User, device string, dir Direction) *Flow {
	if r == nil || u.Email == "" || r.resolve == nil {
		return nil
	}
	now := r.now()

	// The attach happens under the registry lock, not after it. Releasing the
	// lock first would leave a window in which the sweep could evict the very
	// shaper we are about to attach to, and the next connection would build a
	// second one for the same user — two shapers, two caps, twice the plan.
	// attach takes the shaper lock, which is the same order the sweep uses, so
	// holding both here is safe; the work under it is a map lookup and a
	// rebalance over one user's handful of devices.
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sweepLocked(now)
	s := r.users[u.Email]
	if s == nil {
		s = newShaper(u, r.resolve(u), r.now, now)
		r.users[u.Email] = s
	}
	return s.attach(device, dir, now)
}

// Lookup returns the live shaper for a user, or nil if there is none. It does
// not create one: a user with no connection since the last sweep has no state
// worth reading or writing.
func (r *Registry) Lookup(email string) *Shaper {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.users[email]
}

// Invalidate re-resolves one user's limits and applies them to their live
// connections. Call it when a tariff changes.
func (r *Registry) Invalidate(u User) {
	if r == nil || r.resolve == nil {
		return
	}
	r.mu.Lock()
	s := r.users[u.Email]
	r.mu.Unlock()
	if s != nil {
		s.SetLimits(r.resolve(u))
	}
}

// InvalidateAll re-resolves every live user. Call it when a default changes.
func (r *Registry) InvalidateAll() {
	if r == nil || r.resolve == nil {
		return
	}
	r.mu.Lock()
	shapers := make([]*Shaper, 0, len(r.users))
	for _, s := range r.users {
		shapers = append(shapers, s)
	}
	r.mu.Unlock()
	for _, s := range shapers {
		s.SetLimits(r.resolve(s.user))
	}
}

// Len reports how many users currently have shaper state. Used by tests and
// by the management API's health output.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.users)
}

func (r *Registry) sweepLocked(now time.Time) {
	if now.Before(r.nextSweep) {
		return
	}
	r.nextSweep = now.Add(r.sweep)
	for email, s := range r.users {
		if s.evictable(now) {
			delete(r.users, email)
		}
	}
}

// state is the immutable half of a Shaper: everything derived from a Limits.
// It is swapped wholesale under atomic.Pointer so the hot path reads it
// without a lock, and a tariff change is observed atomically rather than
// half-applied.
type state struct {
	limits Limits
	up     *direction
	down   *direction
}

func (s *state) dir(d Direction) *direction {
	if d == Up {
		return s.up
	}
	return s.down
}

// direction is one shaped half of a user's traffic.
type direction struct {
	bps         int64
	chunk       int
	interactive int64

	// steady is the tariff itself: the aggregate cap across every device and
	// connection. Nil means this direction is unlimited, and Wait returns
	// immediately.
	steady *rate.Limiter
	// boost holds the speed-boost budget. Bytes taken from it are free —
	// debited against nothing — so spending the boost cannot leave the user
	// slower afterwards than the tariff they paid for.
	boost *creditBucket
	// ceil paces the boost, when the tariff asks for the boost to be fast but
	// not unbounded. Nil leaves the boost limited only by the path.
	ceil *rate.Limiter
	// prio lends short flows the right to skip the queue. Bytes taken from it
	// are still debited against steady, they simply do not wait for it, so the
	// long-run average stays at the cap.
	prio *creditBucket
}

// device groups the connections that come from one place. The limiters are
// held as atomic pointers because the hot path reads them on every chunk while
// rebalancing rewrites them from under it.
type device struct {
	key  string
	refs int // guarded by Shaper.mu

	up         atomic.Pointer[rate.Limiter]
	down       atomic.Pointer[rate.Limiter]
	lastActive atomic.Int64 // unix nanos
}

func (d *device) limiter(dir Direction) *rate.Limiter {
	if dir == Up {
		return d.up.Load()
	}
	return d.down.Load()
}

func (d *device) setLimiter(dir Direction, l *rate.Limiter) {
	if dir == Up {
		d.up.Store(l)
		return
	}
	d.down.Store(l)
}

func (d *device) active(now time.Time) bool {
	return now.UnixNano()-d.lastActive.Load() < int64(activeWindow)
}

// Shaper enforces one user's policy across all of their connections.
type Shaper struct {
	user User
	now  func() time.Time

	st atomic.Pointer[state]

	// nextRebalanceNano lets the hot path skip the mutex when the shares were
	// recomputed recently.
	nextRebalanceNano atomic.Int64

	mu       sync.Mutex
	devices  map[string]*device
	attached int
	idleAt   time.Time
}

func newShaper(u User, limits Limits, nowFn func() time.Time, now time.Time) *Shaper {
	s := &Shaper{
		user:    u,
		now:     nowFn,
		devices: make(map[string]*device),
		idleAt:  now,
	}
	s.st.Store(buildState(nil, limits, now))
	return s
}

// User reports whose shaper this is.
func (s *Shaper) User() User { return s.user }

// Limits returns the policy currently in force.
func (s *Shaper) Limits() Limits { return s.st.Load().limits }

// SetLimits replaces the policy, taking effect on live connections without
// reconnecting. Credit already accumulated is carried over rather than reset,
// so moving a user between tariffs neither hands them a fresh speed boost nor
// confiscates the one they had.
func (s *Shaper) SetLimits(l Limits) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	st := buildState(s.st.Load(), l, now)
	s.st.Store(st)
	s.rebalanceLocked(now, st)
}

func (s *Shaper) attach(deviceKey string, dir Direction, now time.Time) *Flow {
	s.mu.Lock()
	d := s.devices[deviceKey]
	if d == nil {
		d = &device{key: deviceKey}
		d.lastActive.Store(now.UnixNano())
		s.devices[deviceKey] = d
	}
	d.refs++
	s.attached++
	s.rebalanceLocked(now, s.st.Load())
	s.mu.Unlock()

	return &Flow{s: s, dev: d, dir: dir}
}

func (s *Shaper) release(d *device, now time.Time) {
	s.mu.Lock()
	d.refs--
	if d.refs <= 0 {
		delete(s.devices, d.key)
	}
	s.attached--
	if s.attached <= 0 {
		s.attached = 0
		s.idleAt = now
	}
	s.rebalanceLocked(now, s.st.Load())
	s.mu.Unlock()
}

// maybeRebalance recomputes the per-device shares if enough time has passed.
// It is called from the write path, so the common case must not touch the
// mutex.
func (s *Shaper) maybeRebalance(now time.Time) {
	if now.UnixNano() < s.nextRebalanceNano.Load() {
		return
	}
	s.mu.Lock()
	if now.UnixNano() >= s.nextRebalanceNano.Load() {
		s.rebalanceLocked(now, s.st.Load())
	}
	s.mu.Unlock()
}

// rebalanceLocked divides the cap between the devices that are actually
// moving traffic.
//
// Each active device is capped at Overcommit/N of the user's rate. That cap is
// not what divides the tariff — the user's own bucket does that, and it does
// it evenly because every flow claims equal chunks. The device cap exists so
// that no single device can hold the whole bucket while another is starved,
// and the overcommit above 1 is what lets a device still reach the full tariff
// the moment its neighbours go quiet.
//
// Only devices that have moved a byte within activeWindow are counted, so a
// phone asleep in someone's pocket does not take a third of the household's
// bandwidth with it.
func (s *Shaper) rebalanceLocked(now time.Time, st *state) {
	s.nextRebalanceNano.Store(now.Add(rebalanceInterval).UnixNano())

	active := 0
	for _, d := range s.devices {
		if d.active(now) {
			active++
		}
	}
	if active < 1 {
		active = 1
	}

	for _, dir := range [...]Direction{Up, Down} {
		ds := st.dir(dir)
		if ds.steady == nil {
			for _, d := range s.devices {
				d.setLimiter(dir, nil)
			}
			continue
		}
		share := float64(ds.bps) * st.limits.Overcommit / float64(active)
		if share > float64(ds.bps) {
			share = float64(ds.bps)
		}
		burst := burstFor(int64(share), ds.chunk)
		for _, d := range s.devices {
			if l := d.limiter(dir); l != nil {
				l.SetLimit(rate.Limit(share))
				l.SetBurst(burst)
				continue
			}
			d.setLimiter(dir, rate.NewLimiter(rate.Limit(share), burst))
		}
	}
}

// evictable reports whether the registry may forget this user. See Registry.
func (s *Shaper) evictable(now time.Time) bool {
	s.mu.Lock()
	attached := s.attached
	idleFor := now.Sub(s.idleAt)
	s.mu.Unlock()

	if attached > 0 || idleFor < minIdleEvict {
		return false
	}
	st := s.st.Load()
	for _, ds := range [...]*direction{st.up, st.down} {
		if ds.boost != nil && !ds.boost.full(now) {
			return false
		}
		if ds.prio != nil && !ds.prio.full(now) {
			return false
		}
	}
	return true
}

// Flow is one direction of one connection's view of its user's shaper. The
// zero value is not usable; get one from [Registry.Attach]. A nil Flow is
// valid and does nothing, which is what an unshaped connection gets.
type Flow struct {
	s   *Shaper
	dev *device
	dir Direction

	sent     atomic.Int64
	released atomic.Bool
}

// Wait blocks until the flow is allowed to move n more bytes, or until ctx is
// done — in which case the tokens it had claimed are handed back and ctx's
// error is returned.
//
// It claims the tokens in chunks so that a large write does not park every
// other flow of the same user behind the whole of it; see the package comment.
func (f *Flow) Wait(ctx context.Context, n int) error {
	if f == nil || n <= 0 {
		return nil
	}
	d := f.s.st.Load().dir(f.dir)
	if d.steady == nil { // unlimited in this direction
		return nil
	}

	now := f.s.now()
	f.dev.lastActive.Store(now.UnixNano())
	f.s.maybeRebalance(now)

	// Whether this write may draw on the shared priority allowance is decided
	// once, from the size the caller handed us — before chunking hides it.
	priority := d.prio != nil && n <= maxPriorityWrite

	remaining := n
	for remaining > 0 {
		chunk := remaining
		if chunk > d.chunk {
			chunk = d.chunk
		}
		if err := f.waitChunk(ctx, d, chunk, priority); err != nil {
			// Count what did get through, so a flow interrupted mid-write is
			// not mistaken for a short one on its next attempt.
			f.sent.Add(int64(n - remaining))
			return err
		}
		remaining -= chunk
	}
	f.sent.Add(int64(n))
	return nil
}

// waitChunk claims one chunk. n is guaranteed to be at most the direction's
// chunk size, which is in turn at most every relevant burst, so no reservation
// here can be refused for being too large.
func (f *Flow) waitChunk(ctx context.Context, d *direction, n int, priority bool) error {
	now := f.s.now()

	// 1. The speed boost. These bytes are free: they are not debited against
	//    the tariff, because the point of the boost is to be extra capacity
	//    rather than capacity borrowed from the user's own future.
	if d.boost != nil {
		if k := d.boost.takeUpTo(now, int64(n)); k > 0 {
			if d.ceil != nil {
				if err := waitFor(ctx, now, d.ceil.ReserveN(now, int(k)), nil); err != nil {
					// Hand the credit back: it bought nothing.
					d.boost.refund(k)
					return err
				}
			}
			n -= int(k)
			if n == 0 {
				return nil
			}
			now = f.s.now()
		}
	}

	devLim := f.dev.limiter(f.dir)

	// 2. The priority allowance, for a small write on a flow that has not yet
	//    grown into a transfer. Unlike the boost these bytes are borrowed, not
	//    free: they are debited against both buckets and simply not waited
	//    for, so the buckets go into deficit and the next bulk chunk pays it
	//    back. Short flows therefore jump the queue without the user's average
	//    ever exceeding the cap.
	if priority && f.sent.Load() < d.interactive {
		if k := d.prio.takeUpTo(now, int64(n)); k > 0 {
			d.steady.ReserveN(now, int(k))
			if devLim != nil {
				devLim.ReserveN(now, int(k))
			}
			n -= int(k)
			if n == 0 {
				return nil
			}
		}
	}

	// 3. The tariff. Both reservations are taken before either is waited on,
	//    so the cost is the longer of the two waits rather than their sum.
	return waitFor(ctx, now, d.steady.ReserveN(now, n), reserve(devLim, now, n))
}

func reserve(l *rate.Limiter, now time.Time, n int) *rate.Reservation {
	if l == nil {
		return nil
	}
	return l.ReserveN(now, n)
}

// waitFor sleeps until both reservations mature, cancelling them if ctx ends
// first.
//
// A reservation that is not OK asked for more than its bucket can ever hold.
// That cannot happen with the chunk sizes this package uses, but if it ever
// did the safe answer for a data plane is to let the bytes through unshaped
// rather than to wedge the connection forever, so such a reservation is
// dropped rather than waited on.
func waitFor(ctx context.Context, now time.Time, a, b *rate.Reservation) error {
	delay := time.Duration(0)
	if a != nil && !a.OK() {
		a = nil
	}
	if b != nil && !b.OK() {
		b = nil
	}
	if a != nil {
		delay = a.DelayFrom(now)
	}
	if b != nil {
		if d := b.DelayFrom(now); d > delay {
			delay = d
		}
	}
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		if a != nil {
			a.Cancel()
		}
		if b != nil {
			b.Cancel()
		}
		return ctx.Err()
	}
}

// Release returns the flow's device reference. It is idempotent, so callers
// may wire it to both an explicit Close and a context cancellation without
// having to work out which will fire first.
func (f *Flow) Release() {
	if f == nil || !f.released.CompareAndSwap(false, true) {
		return
	}
	f.s.release(f.dev, f.s.now())
}

// buildState derives the plumbing for a Limits, reusing the live buckets from
// old where it can. Reuse is what makes a tariff change safe to apply under
// traffic: a fresh rate.Limiter would arrive full, handing the user a free
// burst on every change, and a fresh credit bucket would either wipe or
// refund their boost depending on which direction the tariff moved.
func buildState(old *state, l Limits, now time.Time) *state {
	l = l.normalize()
	st := &state{limits: l}
	var oldUp, oldDown *direction
	if old != nil {
		oldUp, oldDown = old.up, old.down
	}
	st.up = buildDirection(oldUp, l, Up, now)
	st.down = buildDirection(oldDown, l, Down, now)
	return st
}

func buildDirection(old *direction, l Limits, dir Direction, now time.Time) *direction {
	if old == nil {
		old = &direction{} // every reusable field nil; the code below builds fresh
	}
	bps := l.bps(dir)
	d := &direction{bps: bps, interactive: l.InteractiveBytes}
	if bps <= 0 {
		return d // unlimited: every field stays nil and Wait short-circuits
	}
	d.chunk = chunkFor(bps)
	burst := burstFor(bps, d.chunk)
	d.steady = reuseLimiter(old.steady, bps, burst)

	if l.BoostBytes > 0 {
		d.boost = reuseBucket(old.boost, l.BoostBytes, l.BoostRefill, now)
		if l.BoostCeilBPS > 0 {
			d.ceil = reuseLimiter(old.ceil, l.BoostCeilBPS, burstFor(l.BoostCeilBPS, d.chunk))
		}
	}
	if l.PriorityShare > 0 {
		capacity, refill := priorityBudget(bps, l.PriorityShare)
		d.prio = reuseBucket(old.prio, capacity, refill, now)
	}
	return d
}

func reuseLimiter(l *rate.Limiter, bps int64, burst int) *rate.Limiter {
	if l == nil {
		return rate.NewLimiter(rate.Limit(bps), burst)
	}
	l.SetLimit(rate.Limit(bps))
	l.SetBurst(burst)
	return l
}

func reuseBucket(b *creditBucket, capacity int64, refill time.Duration, now time.Time) *creditBucket {
	if b == nil {
		return newCreditBucket(capacity, refill, now)
	}
	b.resize(now, capacity, refill)
	return b
}

// chunkFor returns how many bytes one reservation may claim: about 20 ms of
// the user's rate, floored so slow plans do not reserve per packet and capped
// so fast ones do not rebuild the queueing latency chunking exists to avoid.
func chunkFor(bps int64) int {
	c := bps / chunkDivisor
	if c < minChunk {
		c = minChunk
	}
	if c > maxChunk {
		c = maxChunk
	}
	return int(c)
}

// burstFor returns a bucket depth of about a quarter second of traffic, never
// smaller than four chunks — which is what guarantees a chunk reservation can
// always be satisfied — and never larger than maxBurst.
func burstFor(bps int64, chunk int) int {
	b := bps / 4
	if floor := int64(chunk) * 4; b < floor {
		b = floor
	}
	if b > maxBurst {
		b = maxBurst
	}
	return int(b)
}

// priorityBudget sizes the short-flow allowance: a quarter second of the
// user's traffic, between 64 KiB and 1 MiB, replenished at PriorityShare of
// their rate.
func priorityBudget(bps int64, share float64) (capacity int64, refill time.Duration) {
	capacity = bps / 4
	if capacity < 64*1024 {
		capacity = 64 * 1024
	}
	if capacity > 1024*1024 {
		capacity = 1024 * 1024
	}
	perSec := float64(bps) * share
	if perSec <= 0 {
		return capacity, 0
	}
	return capacity, time.Duration(float64(capacity) / perSec * float64(time.Second))
}
