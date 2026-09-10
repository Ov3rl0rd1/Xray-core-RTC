package dispatcher

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/shaper"
	"github.com/xtls/xray-core/features/stats"
)

// The fork's per-user hook, called from getLink.
//
// The dispatcher is the one point every proxied connection crosses, whatever
// protocol carried it in and whichever of the user's devices sent it, so
// everything applied here is automatically uniform across VLESS, Hysteria,
// olcRTC and everything else, and aggregate across all of a user's devices at
// once. That is the whole reason a single line in upstream's getLink is worth
// maintaining.
//
// Three things hang off it:
//
//   - traffic shaping (the cap, the fair split between devices, the speed
//     boost, the priority allowance) — see common/shaper;
//   - per-user-per-inbound accounting — see fork_usage.go;
//   - enforcement of whatever the policy store decides, including refusing
//     traffic outright.
//
// This file only chooses the policy and wires the writers up. It knows nothing
// about quotas or subscriptions; app/tariff installs itself through
// SetLimitsResolver and SetUsageTracker.

// tiers maps a user's Xray level to a plan, for a server with no policy store
// installed.
//
// Level is a good key because it already exists: it is part of every Xray
// user, it is already settable per user at runtime through HandlerService (the
// same AddUserOperation that creates the user), and it already reaches this
// function on every connection. So a panel can put a user on a different plan
// with no extra API and no restart, simply by creating them at the right
// level.
//
// On the boost budgets: what bounds a boost is its refill rate, not its
// capacity. A 1.5 GB budget that takes an hour to refill can add at most
// 1.5 GB/h — about 3 Mbit/s — on top of the tariff no matter how the user
// behaves, while still letting a fresh session run at the ceiling for the
// first minute, which is the part a user actually notices.
var tiers = map[uint32]shaper.Limits{
	// Level 0 — the level every user gets unless someone says otherwise.
	0: {
		UplinkBPS:    50 * mbit,
		DownlinkBPS:  50 * mbit,
		BoostCeilBPS: 200 * mbit,
		BoostBytes:   200 * mbit * 60,
		BoostRefill:  time.Hour,
	},
	1: {
		UplinkBPS:    100 * mbit,
		DownlinkBPS:  100 * mbit,
		BoostCeilBPS: 300 * mbit,
		BoostBytes:   300 * mbit * 60,
		BoostRefill:  time.Hour,
	},
	2: {
		UplinkBPS:    200 * mbit,
		DownlinkBPS:  200 * mbit,
		BoostCeilBPS: 500 * mbit,
		BoostBytes:   500 * mbit * 60,
		BoostRefill:  time.Hour,
	},
	// Level 3 — unshaped. An empty Limits disables the shaping wrappers, so
	// these users pay nothing for the feature existing.
	3: {},
}

// mbit is one megabit per second expressed in the bytes per second that
// [shaper.Limits] speaks, so the table above reads in the units a plan is
// actually sold in.
const mbit = 1_000_000 / 8

// defaultTier applies to a level with no entry in tiers. It matches level 0:
// an unrecognised level should mean "the ordinary plan", never "unlimited".
var defaultTier = tiers[0]

// resolver is the seam through which per-user policy is supplied. It starts as
// the level table above and is replaced by [SetLimitsResolver] once a policy
// store is present.
var resolver atomic.Pointer[func(shaper.User) shaper.Limits]

// SetLimitsResolver installs the source of per-user traffic policy, replacing
// the built-in level table. Live connections pick the new policy up on their
// next write, and every existing shaper is re-resolved immediately.
//
// It exists so that a policy store — one fed by a management API, aware of
// quotas and expiry — can take over without this file having to know about it.
func SetLimitsResolver(fn func(shaper.User) shaper.Limits) {
	if fn == nil {
		resolver.Store(nil)
	} else {
		resolver.Store(&fn)
	}
	shapers.InvalidateAll()
}

// LimitsFor returns the traffic policy in force for a user.
func LimitsFor(u shaper.User) shaper.Limits {
	if fn := resolver.Load(); fn != nil {
		return (*fn)(u)
	}
	return TierLimits(u)
}

// TierLimits returns the built-in level table's plan for a user, whatever
// resolver is installed.
//
// It stays reachable so that a policy store can fall back to it for users it
// has no plan for. Without that, installing a store would silently unshape
// every user the panel had not yet pushed — which is exactly the moment a
// server is most likely to be misconfigured.
func TierLimits(u shaper.User) shaper.Limits {
	if l, ok := tiers[u.Level]; ok {
		return l
	}
	return defaultTier
}

// InvalidateUserLimits re-resolves one user's policy and applies it to their
// live connections. A policy store calls it when a plan changes or a quota
// runs out.
func InvalidateUserLimits(email string) {
	if s := shapers.Lookup(email); s != nil {
		s.SetLimits(LimitsFor(s.User()))
	}
}

// shapers holds the live per-user shaping state.
//
// It is process-wide rather than per-Xray-instance. That is the right domain
// for it — a user's plan is a property of the user, not of which core instance
// happens to be serving them — and it keeps the hook to the single line in
// getLink that this fork's patch adds. Idle users are swept out of it
// automatically; see [shaper.Registry].
var shapers = shaper.New(shaper.Config{Resolve: LimitsFor})

// rateLimitLink wraps a link's writers with everything the fork applies per
// user. up is client to internet, down is internet to client. When nothing is
// configured that would touch this user's traffic, the writers are returned
// untouched, so the feature costs nothing at all when it is not in use.
func rateLimitLink(ctx context.Context, d *DefaultDispatcher, user *protocol.MemoryUser, up, down buf.Writer) (buf.Writer, buf.Writer) {
	su := shaper.User{Email: user.Email, Level: user.Level}
	device := deviceKey(ctx)
	tag := inboundTag(ctx)

	// The meter comes first. Attaching it is what makes the policy store
	// publish this user's current enforcement — their plan, minus whatever
	// their quotas have taken away — and the shaper reads that a few lines
	// below. The other order would shape a user's first connection by the
	// default plan.
	meter := attachMeter(user.Email, user.Level, tag, device)
	if meter != nil {
		// The meter is shared by both directions, so it is released when the
		// connection's context ends rather than by either writer's Close.
		context.AfterFunc(ctx, meter.Release)
	}

	upCounter, downCounter := inboundCounters(d.stats, user.Email, tag)

	var upFlow, downFlow *shaper.Flow
	if !LimitsFor(su).Unlimited() {
		upFlow = shapers.Attach(su, device, shaper.Up)
		downFlow = shapers.Attach(su, device, shaper.Down)
	}

	up = wrapUser(ctx, up, shaper.Up, upFlow, meter, upCounter)
	down = wrapUser(ctx, down, shaper.Down, downFlow, meter, downCounter)
	return up, down
}

func wrapUser(ctx context.Context, w buf.Writer, dir shaper.Direction, flow *shaper.Flow, meter UsageMeter, counter stats.Counter) buf.Writer {
	if flow == nil && meter == nil && counter == nil {
		return w
	}
	if flow != nil {
		// Releasing on context cancellation as well as on Close is not belt
		// and braces: a link can be abandoned without either of its writers
		// being closed, and a device reference leaked that way would keep
		// counting towards the user's fair split forever. Release is
		// idempotent.
		context.AfterFunc(ctx, flow.Release)
	}
	return &userWriter{ctx: ctx, writer: w, dir: dir, flow: flow, meter: meter, counter: counter}
}

// deviceKey identifies which of a user's devices a connection came from, for
// the purpose of splitting their cap fairly between the people sharing a
// subscription and of counting how many devices they have connected.
//
// For socket inbounds that is the source IP, which is the only device identity
// a stock client offers — good enough, since two people sharing a subscription
// are almost always behind different addresses, and a single device changing
// address (a phone moving between cells) merely re-enters under a new key.
// Protocols that carry a real device identifier can do better; olcRTC's
// handshake already has one.
func deviceKey(ctx context.Context) string {
	if inb := session.InboundFromContext(ctx); inb != nil && inb.Source.Address != nil {
		return inb.Source.Address.String()
	}
	return "-"
}

// inboundTag reports which inbound a connection arrived on, which is how a
// per-profile allowance is scoped.
func inboundTag(ctx context.Context) string {
	if inb := session.InboundFromContext(ctx); inb != nil {
		return inb.Tag
	}
	return ""
}

// userWriter applies the fork's per-user policy to one direction of one
// connection: it refuses the write if the user is out of allowance, waits
// until their plan permits the bytes, and counts what actually got through.
//
// The resulting back-pressure propagates through the pipe and slows the
// sender, which is what turns a token bucket into a speed limit.
type userWriter struct {
	ctx     context.Context //nolint:containedctx // the writer interface carries no ctx
	writer  buf.Writer
	dir     shaper.Direction
	flow    *shaper.Flow
	meter   UsageMeter
	counter stats.Counter
}

func (w *userWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	n := int(mb.Len())
	if n > 0 {
		if w.meter != nil && w.meter.Blocked() {
			buf.ReleaseMulti(mb)
			return errors.New("dispatcher: traffic refused: ", w.meter.BlockReason())
		}
		if err := w.flow.Wait(w.ctx, n); err != nil {
			buf.ReleaseMulti(mb)
			return err
		}
	}
	if err := w.writer.WriteMultiBuffer(mb); err != nil {
		return err
	}
	// Counted after the write, so the ledger records what was carried rather
	// than what was attempted.
	if n > 0 {
		if w.counter != nil {
			w.counter.Add(int64(n))
		}
		if w.meter != nil {
			w.meter.Count(w.dir, int64(n))
		}
	}
	return nil
}

func (w *userWriter) Close() error {
	w.flow.Release()
	return common.Close(w.writer)
}

func (w *userWriter) Interrupt() {
	w.flow.Release()
	common.Interrupt(w.writer)
}
