package tariff

import (
	"sync/atomic"
	"time"
)

// Windows are aligned to the calendar in UTC rather than to whenever the user
// first connected. A "monthly" quota that reset one month after a user's first
// byte would drift against every billing system it is meant to mirror, and
// would give two users on the same plan different reset days for no reason.
//
// UTC specifically, not the server's local zone: a fleet spread across regions
// must agree on when the month turned over, and a host whose timezone changes
// under it must not hand everyone a fresh allowance.
func windowBounds(w Window, now time.Time) (start, end time.Time) {
	now = now.UTC()
	switch w {
	case Window_WINDOW_DAY:
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 1)
	case Window_WINDOW_WEEK:
		// ISO weeks start on Monday; Go's Weekday puts Sunday at 0.
		offset := (int(now.Weekday()) + 6) % 7
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -offset)
		end = start.AddDate(0, 0, 7)
	case Window_WINDOW_MONTH:
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	default: // Window_WINDOW_LIFETIME
		return time.Time{}, time.Time{}
	}
	return start, end
}

// defaultWarnPercent is when a quota announces itself before it bites. Late
// enough not to be noise, early enough that a panel can warn a user or a
// human can react.
const defaultWarnPercent = 90

// quotaBucket is one live quota: its terms, and how much of it has been spent
// in the current window.
//
// The counters are atomics because they are written from every connection's
// write path. Nothing here takes a lock and nothing here reads the clock —
// window rollover is handled by [quotaBucket.roll], which the manager calls
// from its sweep. A monthly quota that keeps counting for a few seconds past
// midnight on the first is not worth a time.Now on every write.
type quotaBucket struct {
	spec *Quota

	used        atomic.Uint64
	windowStart atomic.Int64 // unix seconds; 0 for a lifetime window
	exceeded    atomic.Bool
	warned      atomic.Bool
}

func newQuotaBucket(spec *Quota, now time.Time) *quotaBucket {
	b := &quotaBucket{spec: spec}
	if start, _ := windowBounds(spec.GetWindow(), now); !start.IsZero() {
		b.windowStart.Store(start.Unix())
	}
	return b
}

// matches reports whether traffic through inboundTag counts towards this quota.
// A quota with no tag counts everything.
func (b *quotaBucket) matches(inboundTag string) bool {
	tag := b.spec.GetInboundTag()
	return tag == "" || tag == inboundTag
}

// add records n bytes and reports whether the quota just crossed its warning
// threshold or its limit, each at most once per window. Both are reported so
// the caller can emit the event and re-evaluate enforcement; a quota with no
// limit reports neither and is a pure meter.
func (b *quotaBucket) add(n uint64) (warned, exceeded bool) {
	used := b.used.Add(n)
	limit := b.spec.GetLimitBytes()
	if limit == 0 {
		return false, false
	}
	if used >= limit {
		return false, b.exceeded.CompareAndSwap(false, true)
	}
	if used >= limit/100*uint64(b.warnPercent()) {
		return b.warned.CompareAndSwap(false, true), false
	}
	return false, false
}

func (b *quotaBucket) warnPercent() uint32 {
	if p := b.spec.GetWarnPercent(); p > 0 && p <= 100 {
		return p
	}
	return defaultWarnPercent
}

// roll advances the bucket to the window containing now, clearing the counter
// if the window changed. It reports whether it reset.
func (b *quotaBucket) roll(now time.Time) bool {
	start, _ := windowBounds(b.spec.GetWindow(), now)
	if start.IsZero() {
		return false // lifetime quotas never roll
	}
	prev := b.windowStart.Load()
	if prev == start.Unix() {
		return false
	}
	b.windowStart.Store(start.Unix())
	b.used.Store(0)
	b.exceeded.Store(false)
	b.warned.Store(false)
	return true
}

// reset clears the counter and re-arms the quota inside the current window,
// without waiting for the window to turn over.
func (b *quotaBucket) reset(now time.Time) {
	if start, _ := windowBounds(b.spec.GetWindow(), now); !start.IsZero() {
		b.windowStart.Store(start.Unix())
	}
	b.used.Store(0)
	b.exceeded.Store(false)
	b.warned.Store(false)
}

// isExceeded reports whether the limit has been reached in the current window.
func (b *quotaBucket) isExceeded() bool {
	return b.spec.GetLimitBytes() > 0 && b.exceeded.Load()
}

// state renders the bucket for the management API.
func (b *quotaBucket) state(now time.Time) *QuotaState {
	st := &QuotaState{
		InboundTag: b.spec.GetInboundTag(),
		UsedBytes:  b.used.Load(),
		LimitBytes: b.spec.GetLimitBytes(),
		Window:     b.spec.GetWindow(),
		Action:     b.spec.GetAction(),
		Exceeded:   b.isExceeded(),
	}
	if start, end := windowBounds(b.spec.GetWindow(), now); !start.IsZero() {
		st.WindowStarted = start.Unix()
		st.WindowResets = end.Unix()
	}
	return st
}

// sameTerms reports whether two quota specs describe the same allowance, so
// that a policy update which leaves a quota untouched keeps its spent bytes
// instead of silently handing the user a fresh allowance.
func sameTerms(a, b *Quota) bool {
	return a.GetInboundTag() == b.GetInboundTag() &&
		a.GetLimitBytes() == b.GetLimitBytes() &&
		a.GetWindow() == b.GetWindow() &&
		a.GetAction() == b.GetAction() &&
		a.GetThrottleBps() == b.GetThrottleBps() &&
		a.GetWarnPercent() == b.GetWarnPercent()
}
