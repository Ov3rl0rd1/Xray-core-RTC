package dispatcher

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/shaper"
)

// Per-user traffic shaping, hooked into getLink.
//
// The dispatcher is the one point every proxied connection crosses, whatever
// protocol carried it in and whichever of the user's devices sent it, so a
// policy applied here is automatically uniform across VLESS, Hysteria, olcRTC
// and everything else, and aggregate across all of a user's devices at once.
// That is the whole reason the hook lives in this file rather than inside each
// proxy.
//
// The shaping itself — the aggregate cap, the fair split between devices, the
// speed boost and the priority allowance for short flows — lives in
// [shaper]. This file only decides *which* policy a user gets and wires the
// writers up to it.

// tiers maps a user's Xray level to a tariff.
//
// Level is deliberately the key. It is already part of every Xray user, it is
// already settable per user at runtime through HandlerService (the same
// AddUserOperation that creates the user), and it already reaches this
// function on every connection — so an external panel can put a user on a
// different plan today, with no extra API and no restart, simply by creating
// them at the right level.
//
// The numbers below are a starting point; see [SetLimitsResolver] for
// replacing the whole table with a live policy store.
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
	// Level 3 — unshaped. An empty Limits disables the wrappers entirely, so
	// these users pay nothing at all for the feature existing.
	3: {},
}

// mbit is one megabit per second expressed in the bytes per second that
// [shaper.Limits] speaks, so the table above reads in the units a tariff is
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
// the built-in level table. Live connections pick the new policy up on the
// next call, and InvalidateAll re-applies it to the ones already running.
//
// It exists so that a policy store — one fed by the management API, aware of
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
	if l, ok := tiers[u.Level]; ok {
		return l
	}
	return defaultTier
}

// shapers holds the live per-user state.
//
// It is process-wide rather than per-Xray-instance. That is the right domain
// for it — a user's tariff is a property of the user, not of which core
// instance happens to be serving them — and it keeps the hook to the single
// line in getLink that this fork's patch adds. Idle users are swept out of it
// automatically; see [shaper.Registry].
var shapers = shaper.New(shaper.Config{Resolve: LimitsFor})

// rateLimitLink wraps a link's writers so the user's traffic policy applies to
// them. up is client to internet, down is internet to client. Users whose
// policy shapes neither direction are returned untouched, so the feature costs
// nothing at all when it is not in use.
func rateLimitLink(ctx context.Context, email string, level uint32, up, down buf.Writer) (buf.Writer, buf.Writer) {
	u := shaper.User{Email: email, Level: level}
	if LimitsFor(u).Unlimited() {
		return up, down
	}
	device := deviceKey(ctx)
	up = shapeWriter(ctx, up, shapers.Attach(u, device, shaper.Up))
	down = shapeWriter(ctx, down, shapers.Attach(u, device, shaper.Down))
	return up, down
}

func shapeWriter(ctx context.Context, w buf.Writer, flow *shaper.Flow) buf.Writer {
	if flow == nil {
		return w
	}
	// Releasing on context cancellation as well as on Close is not belt and
	// braces: a link can be abandoned without either of its writers being
	// closed, and a device reference leaked that way would keep counting
	// towards the user's fair split forever. Release is idempotent.
	context.AfterFunc(ctx, flow.Release)
	return &shapedWriter{ctx: ctx, writer: w, flow: flow}
}

// deviceKey identifies which of a user's devices a connection came from, for
// the purpose of splitting their cap fairly between the people sharing a
// subscription.
//
// For socket inbounds that is the source IP, which is the only device identity
// a stock client offers — good enough, since two people sharing a subscription
// are almost always behind different addresses, and a single device changing
// address (a phone moving between cells) merely re-enters the split under a
// new key. Protocols that carry a real device identifier can do better;
// olcRTC's handshake already has one.
func deviceKey(ctx context.Context) string {
	if inb := session.InboundFromContext(ctx); inb != nil && inb.Source.Address != nil {
		return inb.Source.Address.String()
	}
	return "-"
}

// shapedWriter defers each write until the user's policy allows the bytes
// through. The resulting backpressure propagates back through the pipe and
// slows the sender, which is what turns a token bucket into a speed limit.
type shapedWriter struct {
	ctx    context.Context //nolint:containedctx // the writer interface carries no ctx
	writer buf.Writer
	flow   *shaper.Flow
}

func (w *shapedWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	if n := int(mb.Len()); n > 0 {
		if err := w.flow.Wait(w.ctx, n); err != nil {
			buf.ReleaseMulti(mb)
			return err
		}
	}
	return w.writer.WriteMultiBuffer(mb)
}

func (w *shapedWriter) Close() error {
	w.flow.Release()
	return common.Close(w.writer)
}

func (w *shapedWriter) Interrupt() {
	w.flow.Release()
	common.Interrupt(w.writer)
}
