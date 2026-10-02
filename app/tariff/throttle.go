package tariff

import (
	"context"

	"github.com/xtls/xray-core/common/shaper"
	"golang.org/x/time/rate"
)

// inboundThrottle holds one user's traffic through one inbound to a rate, in
// each direction independently, while a per-inbound THROTTLE quota is
// exhausted.
//
// It is deliberately simpler than the shaper: no fair split between devices,
// no boost, no priority allowance. Someone on it has run out of an allowance,
// and the job is to keep that inbound usable at a trickle -- messengers, a page
// explaining why -- not to make it pleasant.
type inboundThrottle struct {
	bps  int64
	up   *rate.Limiter
	down *rate.Limiter
}

// throttleBurst is how many bytes a throttled direction may move at once:
// a tenth of a second at the rate, but never less than a typical TLS record,
// so a small write is never split into dozens of waits.
func throttleBurst(bps int64) int {
	const floor = 16 * 1024
	if b := bps / 10; b > floor {
		return int(b)
	}
	return floor
}

func newInboundThrottle(bps int64) *inboundThrottle {
	burst := throttleBurst(bps)
	return &inboundThrottle{
		bps:  bps,
		up:   rate.NewLimiter(rate.Limit(bps), burst),
		down: rate.NewLimiter(rate.Limit(bps), burst),
	}
}

// throttleAt returns this inbound's throttle at bps, replacing one built for a
// different rate. A lost race just builds a second limiter that is dropped,
// which costs a burst's worth of bytes once.
func (iu *inboundUsage) throttleAt(bps int64) *inboundThrottle {
	if t := iu.throttle.Load(); t != nil && t.bps == bps {
		return t
	}
	t := newInboundThrottle(bps)
	iu.throttle.Store(t)
	return t
}

// wait blocks until n bytes may pass in dir. Large writes are taken in
// burst-sized pieces, because a limiter refuses outright any single request
// bigger than its burst.
func (t *inboundThrottle) wait(ctx context.Context, dir shaper.Direction, n int) error {
	l := t.down
	if dir == shaper.Up {
		l = t.up
	}
	for n > 0 {
		chunk := min(n, l.Burst())
		if err := l.WaitN(ctx, chunk); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}
