# Plans, quotas and usage

`app/tariff` is the fork's per-user policy and usage store: who is on which
plan, how much they have spent against their quotas, how many devices they have
connected, and what happens when any of that runs out. It is driven over gRPC
by an external panel and consulted by the data plane on every write.

**Contents:** [Why not Xray's stats](#why-not-xrays-stats) · [Turning it on](#turning-it-on) ·
[Plans](#plans) · [Quotas](#quotas) · [Devices](#devices) ·
[The API](#the-api) · [Events](#events) · [Persistence](#persistence) ·
[Recipes](#recipes) · [Cost](#cost)

---

## Why not Xray's stats

Xray already counts traffic per user and per inbound. This store exists anyway,
for three reasons.

**Xray's counters can be reset by whoever reads them.** `QueryStats(reset=true)`
is how most panels collect traffic, and it zeroes the counter as a side effect.
Enforcing a monthly quota from a number a monitoring poll can wipe means the
quota silently stops working, and nobody finds out until the month is over.
This store owns its own counters.

**Xray counts the user and the inbound, never the two together.** The cross
product is exactly what a VPN service needs: "twenty gigabytes on the profile
that bypasses the whitelist, twenty terabytes overall" is only expressible if
the server knows how much of a user's traffic came through which inbound —
because the profile *is* an inbound. The dispatcher hook adds those counters
too, under names that extend Xray's own scheme:

```
user>>>alice@example>>>inbound>>>vless-in>>>traffic>>>uplink
```

**Enforcement has to survive the panel being down.** Everything here is held in
memory and snapshotted to disk. The panel pushes changes; it is never in the
path of a packet, and an unreachable panel does not mean an unmetered server.

---

## Turning it on

Add `TariffService` to the API service list. That is the whole configuration —
listing it is what creates the store and attaches it to the dispatcher.

```json
{
  "api": {
    "tag": "api",
    "services": ["HandlerService", "StatsService", "TariffService"]
  }
}
```

Set `XRAY_TARIFF_STATE` to a writable path so usage survives a restart:

```bash
XRAY_TARIFF_STATE=/var/lib/xray/tariff.json xray run -c config.json
```

Everything else — plans, quotas, the server allowance — arrives over gRPC and is
persisted with the counters. There is deliberately no JSON config key for any of
it: the panel owns this, not a file on the host, and it keeps the feature out of
upstream's config structs entirely.

**A server that does not list the service carries none of this** and keeps the
plain level-based plans described in
[`common/shaper/README.md`](../../common/shaper/README.md).

### How it composes with the level table

The store overrides the level table per user; it does not replace it. A user is
resolved in three steps:

1. their own `Policy`, if the panel has pushed one;
2. otherwise `Config.default_policy`, if one is configured;
3. otherwise the dispatcher's level table.

So enabling the store does not unshape everybody the panel has not got to yet —
which is exactly the moment a new server is most likely to be misconfigured.

---

## Plans

A `Policy` is keyed by **email** — which means whatever the panel put in
`User.Email` when it provisioned the user, not necessarily an address. Panels
commonly label users with the subscription UUID, because that is the field
Xray removes users and reports stats by; if yours does, then the UUID is the
key here too and nothing has to change to line the two up.

Level still matters as the fallback: a panel that provisions everyone at
`level: 0` gets level 0's plan for anyone the store has no policy for. Setting
`Level` from the plan is a one-line change on the panel side and needs no API
at all — see [`common/shaper/README.md`](../../common/shaper/README.md).

| Field | Meaning |
|---|---|
| `uplink_bps` / `downlink_bps` | Aggregate caps in **bytes** per second, across every device and protocol. 0 = unlimited. 100 Mbit/s is `12500000`. |
| `overcommit` | How much of the cap one device may hold while others are active. See the shaper's README. |
| `boost_bytes`, `boost_refill_seconds`, `boost_ceil_bps` | The speed boost: a budget, its refill period, and the ceiling while it is being spent. |
| `max_devices`, `device_limit_action` | Concurrent devices, and what to do past the limit. |
| `quotas` | See below. |
| `expires_at` | Unix seconds; the user is refused after this. 0 = never. |
| `disabled` | Refuse the user outright. |

Changes take effect on **live connections**, not just new ones.

---

## Quotas

A quota is a byte allowance measured over a window.

```protobuf
Quota {
  inbound_tag   // empty = all of the user's traffic; otherwise just this inbound
  limit_bytes   // 0 = no limit, making the quota a pure meter
  window        // LIFETIME | DAY | WEEK | MONTH
  action        // NOTIFY | THROTTLE | BLOCK
  throttle_bps  // for THROTTLE; 0 uses the manager's default of 1 Mbit/s
  warn_percent  // fires QUOTA_WARNING at this fraction; 0 uses 90
}
```

**Windows align to the calendar in UTC.** A monthly quota resets at 00:00 on the
first, not one month after the user first connected — otherwise two users on the
same plan would have different reset days for no reason, and the server would
drift against whatever billing system it is meant to mirror. UTC specifically,
so a fleet across regions agrees on when the month turned over.

**Actions:**

- `NOTIFY` — emit an event and carry on. The default, because it is the only
  action that cannot surprise a paying user.
- `THROTTLE` — fall back to `throttle_bps` in both directions and drop the
  speed boost. The user stays connected and stays usable, just slowly.
- `BLOCK` — refuse traffic. Live connections start failing on their **next
  write**, so a quota running out during a download stops that download rather
  than waiting for a reconnect.

**A policy push does not reset a quota.** Spent bytes carry over for every quota
whose terms are unchanged. Without that, a panel syncing on a timer would hand
every user a fresh allowance on every sync, and nobody would notice until the
bandwidth bill arrived. Change a quota's *terms* and it is treated as a
different allowance and starts clean.

### The server allowance

`ServerQuota` is the same shape but counts every user's traffic together, for
when the host itself is metered. Its action applies on top of everyone's own
policy. `ACTION_NOTIFY` is usually what you want for a 20 TB host allowance —
let the panel decide whether to throttle, migrate, or just pay.

---

## Devices

A device is a **source address**, which is the only device identity a stock
client offers. That makes the count an approximation in both directions: a
household behind one NAT looks like one device, and a phone moving between cells
briefly looks like two.

`device_limit_action` therefore **defaults to NOTIFY**. Locking a paying
customer out over a cell handover is a worse failure than letting a shared
subscription through, so the default tells the panel and lets it decide. Set
`ACTION_BLOCK` when you want the server to enforce it.

When it does enforce, only the *extra* device is refused — the ones already
connected keep working — and a refused device is not counted, so a client
retrying cannot ratchet the cap down. Slots are freed when a device's last
connection closes; devices are remembered for five minutes after that so a
client reconnecting every few seconds does not produce an event storm.

> olcRTC connections currently arrive with no per-device address, so all of a
> user's olcRTC traffic counts as one device. Real per-device identity for it is
> tied to the per-user secrets work.

---

## The API

`TariffService`, on the same endpoint as `HandlerService` and `StatsService`.

| RPC | For |
|---|---|
| `SetPolicy` | One user's plan. |
| `SetPolicies` | A whole roster. With `replace`, the server drops users not on the list. |
| `GetPolicy` / `ListPolicies` / `RemovePolicy` | Read and delete. |
| `GetUsage` | What a user (or everyone) has spent: totals, per inbound, per quota, devices, effective caps, whether they are blocked and why. |
| `AddUsage` | Add to the counters without traffic having crossed this server. |
| `ResetUsage` | Clear a user's counters and re-arm their quotas. Empty email resets the server allowance. |
| `SetServerQuota` / `SetConfig` | The host allowance and the manager's own settings. |
| `GetStatus` | Users, connections, devices, the server allowance, the live config. |
| `Flush` | Write the state file now. Worth calling before a planned restart. |
| `StreamEvents` | Changes as they happen. |

The proto ships `csharp_namespace = "Xray.App.Tariff"` and
`"Xray.App.Tariff.Command"`, so `Grpc.Tools` generates idiomatic C# from
[`config.proto`](config.proto) and [`command/command.proto`](command/command.proto)
directly.

### Syncing a roster

`SetPolicies` with `replace: true` is the call a panel should make on its timer:
push everyone, and let the server converge. It removes the need for the panel to
work out what changed, and it means a subscription cancelled while this server
was unreachable does not outlive the next sync.

### Reconciling after a loss

The panel is the ultimate source of truth for what a subscription has spent.
After a state file is lost, a server is rebuilt, or a user moves between
servers, `AddUsage` restores the month-to-date figure. The bytes count towards
every quota they would have counted towards had they really been carried, so
restoring a baseline past the limit blocks the user immediately — which is the
point.

---

## Events

`StreamEvents` pushes changes so the panel does not have to poll to discover
them.

| Kind | Fires when |
|---|---|
| `USER_CONNECTED` / `USER_DISCONNECTED` | First and last connection. |
| `DEVICE_ADDED` / `DEVICE_REMOVED` | A device appears or is forgotten. |
| `DEVICE_LIMIT_HIT` | A device past the cap; `detail` says whether it was refused or allowed. |
| `QUOTA_WARNING` / `QUOTA_EXCEEDED` / `QUOTA_RESET` | Per user, with `inbound_tag` naming the profile. |
| `SERVER_QUOTA_*` | The same for the host allowance. |
| `POLICY_CHANGED` / `POLICY_REMOVED` | The panel's own pushes, echoed back. |
| `USER_EXPIRED` | `expires_at` passed. |

**A stream that stops being read loses events rather than stalling the server.**
Letting a wedged gRPC stream apply back-pressure to the data plane would turn a
monitoring problem into an outage. Every message carries `dropped`, the count
this stream has missed since it started; a non-zero value is the signal to
reconcile with `GetUsage` rather than assume nothing happened.

Filter with `kinds` and `email` to avoid shipping the whole firehose.

---

## Persistence

State is written to `XRAY_TARIFF_STATE` as protojson: the config, every user's
policy and counters, the quota windows and how much of each is spent.

- Written at most every `flush_seconds` (default 10), and only when something
  changed. A hard kill therefore loses at most that much accounting, which for a
  monthly quota is noise.
- Written to a sibling file and renamed over the target, so a crash mid-write
  leaves the previous ledger intact rather than a truncated one.
- **Devices are not persisted.** The only thing they are enforced on is how many
  are connected at once, which is zero after a restart anyway.
- A quota whose window has turned over while the server was down starts clean;
  one still inside its window keeps its spent bytes.
- A quota whose **terms** changed while the server was down is a different
  allowance and starts clean.
- A file written by a newer build is **refused**, loudly. Starting from a
  partial ledger is a bandwidth bill, not something to paper over.

---

## Recipes

### 20 TB on the host, 20 GB on the full-tunnel profile

```csharp
// The host allowance: tell me, do not cut anyone off.
await client.SetServerQuotaAsync(new SetServerQuotaRequest {
    Quota = new ServerQuota {
        LimitBytes = 20L * 1024 * 1024 * 1024 * 1024,
        Window = Window.Month,
        Action = Action.Notify,
        WarnPercent = 80,
    }
});

// A user on 100 Mbit, three devices, with a small allowance on the profile
// that bypasses the whitelist and a large one overall.
await client.SetPolicyAsync(new SetPolicyRequest {
    Policy = new Policy {
        Email = "alice@myapp",
        UplinkBps = 100_000_000 / 8,
        DownlinkBps = 100_000_000 / 8,
        BoostCeilBps = 300_000_000 / 8,
        BoostBytes = (300_000_000 / 8) * 60,   // roughly a minute at the ceiling
        BoostRefillSeconds = 3600,
        MaxDevices = 3,
        DeviceLimitAction = Action.Notify,
        Quotas = {
            new Quota {
                InboundTag = "full-tunnel",
                LimitBytes = 20L * 1024 * 1024 * 1024,
                Window = Window.Month,
                Action = Action.Block,
            },
            new Quota {
                LimitBytes = 500L * 1024 * 1024 * 1024,
                Window = Window.Month,
                Action = Action.Throttle,
                ThrottleBps = 5_000_000 / 8,
            },
        },
        ExpiresAt = DateTimeOffset.UtcNow.AddDays(30).ToUnixTimeSeconds(),
    }
});
```

### Billing days that are not the first of the month

Leave the quota on `WINDOW_LIFETIME` and call `ResetUsage` on the user's own
renewal day. That keeps the billing calendar in the billing system instead of
reimplementing it here.

### Reacting instead of polling

```csharp
using var stream = client.StreamEvents(new StreamEventsRequest {
    Kinds = { EventKind.QuotaExceeded, EventKind.DeviceLimitHit, EventKind.ServerQuotaWarning }
});
await foreach (var msg in stream.ResponseStream.ReadAllAsync(token)) {
    if (msg.Dropped > 0) await ReconcileEverythingAsync();  // we missed some
    await Handle(msg.Event);
}
```

---

## Cost

- **Write path.** Counting a write is a handful of atomic adds against pointers
  resolved once when the connection was set up: no map lookups, no locks, no
  clock reads. The only extra work is comparing each quota against its limit,
  and the only time it does more is the single write on which one crosses a
  threshold.
- **Memory.** One small struct per user, one per device, one per quota. Ten
  thousand users is a few megabytes.
- **Windows** are rolled by a sweep that runs opportunistically when a
  connection is set up, at most every ten seconds — so a monthly quota may keep
  counting for a few seconds past midnight on the first, which is not worth a
  clock read on every write.
- **Users are never evicted.** They only enter the map by authenticating, so the
  map is bounded by the number of real subscriptions. `RemovePolicy` (or a
  replacing `SetPolicies`) is what drops one.

---

## Testing

```bash
go test ./app/tariff/...
go test -race ./app/tariff/...
```

The store's tests drive an injected clock, so month boundaries, expiry dates and
device retention are exercised without waiting for them. The service's tests run
the generated stubs over a real in-process gRPC connection, so the wire types
and the filtering in `StreamEvents` are covered rather than just the Go methods
underneath them.
