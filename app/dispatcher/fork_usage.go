package dispatcher

import (
	"sync/atomic"

	"github.com/xtls/xray-core/common/shaper"
	"github.com/xtls/xray-core/features/stats"
)

// Traffic accounting for the fork's policy store, and the per-user-per-inbound
// counters that make it useful.
//
// Xray already counts a user's total traffic and an inbound's total traffic,
// but never the two together — and the cross product is exactly what a VPN
// service needs. "Twenty gigabytes on the profile that bypasses the whitelist,
// twenty terabytes overall" is only expressible if the server knows how much
// of a user's traffic came through which inbound, because the profile *is* an
// inbound.

// UsageMeter is one connection's handle into whatever is accounting for
// traffic. The dispatcher calls Count for every byte and Blocked before every
// write; the policy store supplies the implementation.
type UsageMeter interface {
	// Count records n bytes moved in one direction. It is called on the write
	// path and must not block.
	Count(dir shaper.Direction, n int64)
	// Blocked reports whether this connection's traffic must be refused. It is
	// read before every write rather than once per connection, so that an
	// allowance running out during a transfer stops that transfer.
	Blocked() bool
	// BlockReason explains a Blocked meter. It is only called on the write
	// that is being refused, so it may be as slow as it likes.
	BlockReason() string
	// Release gives up the connection's references. It is called once, and
	// must tolerate being called again.
	Release()
}

// UsageTracker hands out meters. It is the seam through which the fork's
// policy store attaches itself to the data plane; see app/tariff.
type UsageTracker func(email string, level uint32, inboundTag, device string) UsageMeter

var usageTracker atomic.Pointer[UsageTracker]

// SetUsageTracker installs the traffic accounting. Passing nil removes it, and
// connections then carry no meter at all.
func SetUsageTracker(t UsageTracker) {
	if t == nil {
		usageTracker.Store(nil)
		return
	}
	usageTracker.Store(&t)
}

// attachMeter returns the meter for a connection, or nil when no tracker is
// installed.
func attachMeter(email string, level uint32, inboundTag, device string) UsageMeter {
	t := usageTracker.Load()
	if t == nil {
		return nil
	}
	return (*t)(email, level, inboundTag, device)
}

// inboundCounters returns the (uplink, downlink) counters for one user's
// traffic through one inbound, or nils when stats are off or the connection
// arrived on no inbound.
//
// The names extend Xray's own scheme rather than inventing one, so anything
// that already understands "user>>>alice>>>traffic>>>uplink" can find these by
// pattern too:
//
//	user>>>alice@example>>>inbound>>>vless-in>>>traffic>>>uplink
func inboundCounters(sm stats.Manager, email, inboundTag string) (up, down stats.Counter) {
	if sm == nil || email == "" || inboundTag == "" {
		return nil, nil
	}
	prefix := "user>>>" + email + ">>>inbound>>>" + inboundTag + ">>>traffic>>>"
	up, _ = sm.GetOrRegisterCounter(prefix + "uplink")
	down, _ = sm.GetOrRegisterCounter(prefix + "downlink")
	return up, down
}
