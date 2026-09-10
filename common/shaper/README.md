# Per-user traffic shaping

`common/shaper` decides how much of the server a subscription gets, and how
that is divided between the people sharing it. It is hooked into
[`app/dispatcher/ratelimit.go`](../../app/dispatcher/ratelimit.go), which is
where tariffs are chosen, and it applies to **every protocol at once** — VLESS,
Hysteria, olcRTC, anything — because the dispatcher is the single point every
proxied connection crosses.

**Contents:** [Why not BBR or Brutal](#why-not-bbr-or-brutal) ·
[What it does](#what-it-does) · [Tariffs](#tariffs) ·
[Tuning](#tuning) · [Cost](#cost) · [Plugging in a policy store](#plugging-in-a-policy-store)

---

## Why not BBR or Brutal

They answer a different question. BBR and Brutal are **congestion control**:
they live in the sender's TCP/QUIC stack and work out how fast the *path* can
take data. They have no notion of a user, and Brutal in particular needs the
peer to cooperate — which rules it out here, because the clients are stock Xray
builds (Happ, v2box, …) that this fork does not control. Anything requiring a
patched client breaks the compatibility that makes those clients usable at all.

Deciding how much of the server's capacity a subscription gets, and how it is
split between devices, is a **scheduling** question, and that is what this
package answers. The two are complementary:

| Want | Where it lives |
|---|---|
| Use the path efficiently under loss | `net.ipv4.tcp_congestion_control=bbr` + `net.core.default_qdisc=fq` on the host |
| Hysteria's QUIC congestion control | `streamSettings.finalmask.quicParams.congestion` (already in the tree) |
| Per-user caps, fairness, boost, quotas | **here** |

---

## What it does

### An aggregate cap per user

One token bucket per (user, direction). Every device and every connection of
that user draws from it, so a 100 Mbit plan is 100 Mbit no matter how many
sessions are opened.

### A fair split between devices

The cap alone is not enough when several people share a subscription: whoever
asks first takes it. Each device therefore also gets its own limiter, sized at
`Overcommit / N` of the cap, where `N` counts only devices that have moved a
byte in the last five seconds.

The device limiter is not what divides the plan — the user's own bucket does
that, evenly, because every flow claims equal-sized chunks. The device limiter
exists so no single device can hold the whole bucket while another starves, and
the overcommit above 1 is what lets one device still reach the *full* plan the
moment its neighbours go quiet. With the default `1.5`:

| Devices active | Each capped at | Actually gets |
|---|---|---|
| 1 | 100% of the plan | the whole plan |
| 2 | 75% | ~50% each, and 75% the instant the other pauses |
| 3 | 50% | ~33% each |

A device is identified by source IP, which is the only device identity a stock
client offers. Two people sharing a subscription are almost always behind
different addresses; a single phone moving between cells simply re-enters the
split under a new key.

### A speed boost, not a 60-second timer

A fresh session runs at `BoostCeilBPS` until it has spent `BoostBytes`, then
settles onto the tariff. The budget refills over `BoostRefill` while the user
is idle.

A **budget** rather than a timer is deliberate. A timer measures how long ago
the user connected, which any client can reset by reconnecting — so a
"60 seconds of full speed" timer is worth nothing against someone who wants to
abuse it. A budget measures how much fast traffic they have actually been
given, which they cannot reset.

It also bounds itself in the way that matters. What limits a boost is its
**refill rate, not its capacity**: a 1.5 GB budget that takes an hour to refill
can add at most 1.5 GB/h — about 3 Mbit/s — on top of the tariff no matter how
the user behaves, while still letting a new session run at 200 Mbit for its
first minute. That first minute is the part a user actually notices, and a
speed test lands squarely inside it.

Boost bytes are **free**: they are not debited against the tariff, so spending
the boost never leaves a user slower afterwards than the plan they paid for.

They also bypass the per-device split, which is the honest trade here: a speed
test that got a third of the boost because two other devices were attached
would not be a speed test. The budget is shared per user, so one device can
spend all of it — for the minute or so it lasts.

### A priority allowance for short flows

This is the part that decides whether a shaped connection feels *slow* or feels
*broken*.

The naive implementation — one bucket, one reservation per write — has a nasty
failure mode: a download writing 512 KiB reserves 512 KiB of tokens up front,
and every other flow of that user queues behind **all** of it. A DNS lookup, a
TLS handshake, a keystroke in an SSH session: on a 1 Mbit plan that is four
seconds of added latency. The link is not saturated; it is head-of-line
blocked inside the limiter.

Two mechanisms fix it:

1. **Chunked reservations.** No reservation claims more than about 20 ms of the
   user's rate, so a flow waits behind one chunk per competitor rather than one
   whole write.
2. **The priority allowance.** A small write (≤ 16 KiB) on a flow that has not
   yet moved 128 KiB may take its bytes from a separate credit bucket and skip
   the queue entirely.

Those bytes are **borrowed, not free**: they are still debited against the
tariff, they simply do not wait for it. The bucket goes into deficit and the
next bulk chunk pays it back, so short flows stay responsive while the user's
long-run average stays exactly at the cap.

Eligibility deliberately tests the *write size* as well as the flow's total.
The allowance is shared between all of a user's flows, and a cumulative test
alone would let a download drain the whole thing while spending its own
short-flow quota.

---

## Tariffs

Plans are keyed by the user's **Xray level**, in `tiers` in
[`ratelimit.go`](../../app/dispatcher/ratelimit.go).

Level is a good key because it already exists: it is part of every Xray user,
it is already settable per user at runtime through `HandlerService`
(the same `AddUserOperation` that creates the user), and it already reaches the
dispatcher on every connection. So a panel can put a user on a different plan
**today**, with no extra API and no restart, just by creating them at the right
level.

| Level | Plan | Boost |
|---|---|---|
| 0 | 50 Mbit up / 50 Mbit down | 200 Mbit for the first minute |
| 1 | 100 / 100 | 300 Mbit |
| 2 | 200 / 200 | 500 Mbit |
| 3 | unshaped | — |

An unrecognised level gets level 0's plan — an unknown level must mean "the
ordinary plan", never "unlimited".

Level 3 is an empty `Limits`, which disables the writer wrappers outright, so
unshaped users pay nothing at all for the feature existing.

### Setting a user's level

```bash
# add_user.json — level picks the plan
{ "inbounds": [ { "tag": "vless-in", "protocol": "vless", "settings": {
  "clients": [ { "email": "alice@myapp", "id": "<uuid>", "level": 2 } ] } } ] }

xray api adu --server=127.0.0.1:10085 add_user.json
```

Changing a live user's plan means removing and re-adding them at the new level;
their existing connections keep the old plan until they reconnect. A policy
store (below) removes that restriction.

---

## Tuning

Everything below is a field on `shaper.Limits`.

| Field | Default | What it changes |
|---|---|---|
| `UplinkBPS` / `DownlinkBPS` | 0 (unlimited) | The plan, in **bytes** per second. Zero disables shaping for that direction. |
| `Overcommit` | `1.5` | How much of the cap one device may hold while others are active. `1.0` is a strict equal split with no reuse of idle capacity; higher favours utilisation over fairness. |
| `BoostBytes` | 0 (off) | Boost budget in bytes. `BoostCeilBPS × 60` gives a one-minute boost. |
| `BoostRefill` | 1 h | How long an idle user takes to earn the budget back. **This is what bounds the boost's cost**, not `BoostBytes`. |
| `BoostCeilBPS` | 0 (unbounded) | Ceiling while the boost is being spent. Leave at 0 only if one user saturating the server's uplink is acceptable. |
| `PriorityShare` | `0.10` | Fraction of the plan lent to short flows. Negative disables the allowance. |
| `InteractiveBytes` | 128 KiB | How much a flow may move before it stops counting as short. |

Algorithm constants (chunk sizing, the activity window, eviction) are in
`shaper.go` and are properties of the algorithm rather than of any plan.

### Symptoms → knob

| Symptom | Try |
|---|---|
| Pages feel sluggish while something downloads | Raise `PriorityShare`, or raise `InteractiveBytes` if the short things are large |
| One device starves the others | Lower `Overcommit` towards `1.0` |
| A single user's speed test is disappointing | Raise `BoostCeilBPS`; raise `BoostBytes` to match |
| Boost traffic is costing too much | Lengthen `BoostRefill` — capacity is not the lever |
| Throughput sits below the plan on a fast link | The plan is in **bytes**/s, not bits: 100 Mbit is `100 * 1_000_000 / 8` |

---

## Cost

- **Memory.** One `Shaper` per user (a handful of small structs and two
  buckets per direction), plus one limiter per active device. Ten thousand
  users is a few megabytes. Idle users are swept automatically.
- **CPU under the cap.** One atomic load and two token-bucket reservations per
  chunk. Negligible beside the AEAD encryption on the same bytes.
- **CPU over the cap.** The writer goroutine parks on a timer. Back-pressure
  and latency, not spin.
- **Contention.** Per user, not global: the registry lock is taken once per
  connection and once per sweep. Two users never contend.

### When a user is forgotten

Shapers outlive the connections that created them, deliberately: if
disconnecting forgot a user, a client could reconnect to refill its boost and
the whole budget would be worth nothing.

A user is dropped once they have had **no connection for at least five
minutes** *and* **every credit bucket has refilled** — at which point
remembering them and recreating them are indistinguishable, so forgetting them
is free. In practice a heavy user is remembered for up to `BoostRefill` after
they disconnect.

The sweep runs opportunistically inside `Attach`, so the registry needs no
goroutine and no shutdown.

---

## Plugging in a policy store

The level table is a floor, not a ceiling. This fork ships such a store —
[`app/tariff`](../../app/tariff/README.md), which adds quotas, expiry dates,
device caps and a gRPC API — and it installs itself with one call:

```go
dispatcher.SetLimitsResolver(func(u shaper.User) shaper.Limits {
    return myStore.LimitsFor(u.Email, u.Level)
})
```

Live connections pick the new policy up on their next write, and existing
shapers are re-resolved immediately. To push a change for one user without
touching the rest, call `SetLimits` on their shaper directly.

Because the resolver returns *effective* limits, quota enforcement needs no
support here at all: a store that wants to throttle a user who has burned
through their monthly allowance simply returns a slower `Limits` for them.

---

## Testing

```bash
go test ./common/shaper/          # ~3 s
go test -race ./common/shaper/
```

The tests cover the credit buckets and eviction on an injected clock, and the
throughput, fairness, boost and priority behaviour against real time with
generous margins. `TestChunkAlwaysFitsInBurst` guards the invariant the whole
reservation path depends on: if a chunk could ever exceed a bucket, the
reservation would be refused and shaping would silently stop.
