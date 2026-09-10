package tariff

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/shaper"
)

// effective is the enforcement in force for one user right now: the shaping
// their traffic gets, and whether it is allowed at all.
//
// It is rebuilt whenever something that feeds it changes — a policy update, a
// quota crossing its limit, a window rolling over, the clock passing an expiry
// — and published as one immutable value, so the write path reads it with a
// single atomic load and never sees a half-applied decision.
type effective struct {
	limits  shaper.Limits
	blocked bool
	reason  string
}

// inboundUsage is a user's traffic through one inbound. Separating traffic by
// inbound is what makes "a small allowance on the profile that bypasses the
// whitelist, a large one overall" expressible: the profile is an inbound, and
// a quota naming it counts only that traffic.
type inboundUsage struct {
	tag  string
	up   atomic.Uint64
	down atomic.Uint64
}

// deviceState is one source of a user's traffic.
//
// A device is kept for a while after its last connection closes so that a
// client which reconnects every few seconds does not produce an event storm,
// and so the management API can still report who was recently on. Only devices
// with a live connection count towards max_devices.
type deviceState struct {
	key       string
	firstSeen int64
	conns     int          // guarded by userState.mu
	lastSeen  atomic.Int64 // unix nanos, written from the write path
}

// userState is everything the manager knows about one user.
//
// The split between the mutex and the atomics is the point: the write path
// touches only atomics, through pointers a [Meter] resolved once when the
// connection was set up, so counting a write costs a handful of adds and no
// locks at all. The mutex guards the shape of things — which quotas exist,
// which devices are connected — which only changes when a connection opens or
// closes or a policy is pushed.
type userState struct {
	email string
	level atomic.Uint32

	up   atomic.Uint64
	down atomic.Uint64

	eff atomic.Pointer[effective]

	mu       sync.Mutex
	policy   *Policy // nil means the manager's default
	inbounds map[string]*inboundUsage
	quotas   []*quotaBucket
	devices  map[string]*deviceState
	conns    int
	lastSeen time.Time
}

func newUserState(email string, level uint32, now time.Time) *userState {
	u := &userState{
		email:    email,
		inbounds: make(map[string]*inboundUsage),
		devices:  make(map[string]*deviceState),
		lastSeen: now,
	}
	u.level.Store(level)
	u.eff.Store(&effective{})
	return u
}

// inbound returns the counter for a tag, creating it on first use. Called once
// per connection, not per write.
func (u *userState) inbound(tag string) *inboundUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	iu := u.inbounds[tag]
	if iu == nil {
		iu = &inboundUsage{tag: tag}
		u.inbounds[tag] = iu
	}
	return iu
}

// setPolicyLocked installs a policy, carrying over the spent bytes of every
// quota whose terms are unchanged.
//
// Carrying them over is what stops a policy push from being a way to reset a
// quota. A panel that syncs policies on a timer would otherwise hand every user
// a fresh monthly allowance on every sync, and nobody would notice until the
// bandwidth bill arrived.
func (u *userState) setPolicyLocked(p *Policy, now time.Time) {
	old := u.quotas
	u.policy = p

	specs := p.GetQuotas()
	fresh := make([]*quotaBucket, 0, len(specs))
	for _, spec := range specs {
		var carried *quotaBucket
		for _, b := range old {
			if sameTerms(b.spec, spec) {
				carried = b
				break
			}
		}
		if carried != nil {
			carried.spec = spec
			fresh = append(fresh, carried)
			continue
		}
		fresh = append(fresh, newQuotaBucket(spec, now))
	}
	u.quotas = fresh
}

// quotasFor returns the buckets that traffic through inboundTag counts towards.
// Resolved once per connection so the write path holds the pointers directly.
func (u *userState) quotasFor(inboundTag string) []*quotaBucket {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []*quotaBucket
	for _, b := range u.quotas {
		if b.matches(inboundTag) {
			out = append(out, b)
		}
	}
	return out
}

// activeDevicesLocked counts devices with a live connection. Devices lingering
// after their last connection closed do not count against max_devices — only
// what is connected now does.
func (u *userState) activeDevicesLocked() uint32 {
	n := uint32(0)
	for _, d := range u.devices {
		if d.conns > 0 {
			n++
		}
	}
	return n
}

// limitsFrom maps a policy onto the shaper's vocabulary.
func limitsFrom(p *Policy) shaper.Limits {
	l := shaper.Limits{
		UplinkBPS:    int64(p.GetUplinkBps()),
		DownlinkBPS:  int64(p.GetDownlinkBps()),
		Overcommit:   p.GetOvercommit(),
		BoostBytes:   int64(p.GetBoostBytes()),
		BoostCeilBPS: int64(p.GetBoostCeilBps()),
	}
	if s := p.GetBoostRefillSeconds(); s > 0 {
		l.BoostRefill = time.Duration(s) * time.Second
	}
	return l
}

// applyAction folds one exhausted quota's consequence into an effective.
func applyAction(e *effective, a Action, throttleBPS int64, reason string) {
	switch a {
	case Action_ACTION_BLOCK:
		e.blocked = true
		e.reason = reason
	case Action_ACTION_THROTTLE:
		e.limits.UplinkBPS = throttleTo(e.limits.UplinkBPS, throttleBPS)
		e.limits.DownlinkBPS = throttleTo(e.limits.DownlinkBPS, throttleBPS)
		// Someone who is over their allowance should not also be handed a
		// speed boost; that would make the first minute after exhausting a
		// quota the fastest part of the month.
		e.limits.BoostBytes = 0
		e.reason = reason
	case Action_ACTION_NOTIFY:
		// The event has already been emitted. Nothing changes here, which is
		// the point: the panel decides.
	}
}

// throttleTo lowers a cap, treating the shaper's "0 means unlimited" correctly
// — an unlimited user being throttled ends up at the throttle rate, not at
// unlimited.
func throttleTo(current, throttle int64) int64 {
	if throttle <= 0 {
		return current
	}
	if current == 0 || current > throttle {
		return throttle
	}
	return current
}
