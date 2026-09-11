# olcRTC proxy for Xray

`proxy/olcrtc` integrates the [olcRTC](https://github.com/openlibrecommunity/olcrtc)
encrypted TCP‑over‑WebRTC tunnel into this Xray fork as a first‑class proxy
protocol, with **both** a client (outbound) and a server (inbound).

Traffic is disguised as an ordinary video call on an allowed SFU service
(Yandex Telemost, WbStream) and additionally encrypted end‑to‑end. Inside the
call it multiplexes many TCP connections (smux) over the WebRTC video channel.

The server holds a long‑term X25519 key pair and clients carry only its public
half, the same shape as REALITY and WireGuard. Every connection negotiates a
session key of its own before anything else flows — see
[Keys & identity](#keys--identity).

```
app ─▶ Xray (socks/vless/…) ─▶ olcrtc outbound  ══WebRTC/SFU══▶  olcrtc inbound ─▶ Xray router ─▶ internet
                                (client / cnc)                    (server / srv)
```

**Contents:** [Roles](#roles) · [Settings reference](#settings-reference) ·
[Providers](#providers) · [Transports & speed](#transports--speed) ·
[Keys & identity](#keys--identity) · [Speed limiting](#speed-limiting) ·
[Connected users](#connected-users) · [Example configs](#example-configs) ·
[Embedded usage](#embedded-usage) · [Roadmap](#roadmap--planned-follow-ups) ·
[Caveats](#caveats).

---

## Roles

| Xray role | olcrtc role | Config message | JSON `protocol` |
|-----------|-------------|----------------|-----------------|
| `outbounds[]` | client (`cnc`) | `ClientConfig` | `olcrtc` |
| `inbounds[]`  | server (`srv`) | `ServerConfig` | `olcrtc` |

The **inbound is self‑driven**: it has no listening socket and needs no `port`
or `listen`. It dials out to the SFU room, accepts tunnel streams and dispatches
each target through Xray's router — so routing rules, DNS, domain sniffing,
stats and the chosen egress outbound (`freedom`, chaining, etc.) all apply on
the server, exactly like a normal inbound.

The server is configured with **`privateKey`**; every client with the matching
**`publicKey`**. Both sides must name the same **`roomId`** and use the same
**`provider`** + **`transport`**.

---

## Settings reference

The `settings` object is identical for inbound and outbound except for the two
client‑only `deviceId*` fields. Unset tuning fields fall back to the defaults
shown below.

### Core (both sides)

| Field | Type | Req. | Default | Notes |
|-------|------|:----:|---------|-------|
| `provider` | string | ✅ | — | `telemost`, `wbstream`, or `none`. See [Providers](#providers). |
| `transport` | string | ✅ | — | `vp8channel` or `seichannel`. See [Transports](#transports--speed). |
| `roomId` | string | ✅¹ | — | Room reference for the provider. ¹Required unless `provider:"none"`. Telemost/WbStream: room ID created on the service site. |
| `privateKey` | string | ✅ (inbound) | — | The server's X25519 private key, from `xray x25519`. |
| `publicKey` | string | ✅ (outbound) | — | The server's X25519 public key. Not a secret. |
| `dnsServer` | string | — | system | Resolver used to reach the SFU, e.g. `8.8.8.8:53`. |
| `authToken` | string | — | — | Provider account token (mainly WbStream). See [Providers](#providers). |

### Direct engine mode (only when `provider:"none"`)

| Field | Type | Notes |
|-------|------|-------|
| `engine` | string | `livekit` or `goolom`. |
| `url` | string | Signaling/SFU URL. |
| `token` | string | Pre‑issued engine token/JWT. |

### `vp8` object — vp8channel tuning

| Field | Type | Default | Notes |
|-------|------|:-------:|-------|
| `fps` | int | `30` | VP8 stream FPS. **Lower = less CPU.** |
| `batchSize` | int | `64` | Frames per tick. **Larger = higher throughput** (more CPU/latency). |

### `sei` object — seichannel tuning

| Field | Type | Default | Notes |
|-------|------|:-------:|-------|
| `fps` | int | `30` | H.264 stream FPS. |
| `batchSize` | int | `64` | Frames per tick. |
| `fragmentSize` | int | `900` | Payload fragment size (bytes). |
| `ackTimeoutMs` | int | `2000` | ACK timeout (ms) before retransmit. |

### `video` object — videochannel tuning (not accepted by this build)

| Field | Type | Default | Notes |
|-------|------|:-------:|-------|
| `codec` | string | `qrcode` | `qrcode` or `tile`. |
| `width` / `height` | int | `1920` / `1080` | For `codec:"tile"` **exactly `1080`×`1080`** is required. |
| `fps` | int | `30` | |
| `bitrate` | string | `"2M"` | e.g. `"5000k"`, `"2M"`. Higher helps throughput at a CPU/detectability cost. |
| `hw` | string | `none` | `none` or `nvenc` (NVIDIA hardware encode). |
| `qrRecovery` | string | `low` | QR error correction: `low`/`medium`/`high`/`highest`. |
| `qrSize` | int | `0` (auto) | QR fragment size (bytes). |
| `tileModule` | int | `4` | Tile size px 1..270 (`tile` only). |
| `tileRs` | int | `20` | Reed‑Solomon parity % 0..200 (`tile` only). |

### Liveness & lifecycle (both sides)

| Field | Type | Default | Notes |
|-------|------|:-------:|-------|
| `livenessInterval` | string | `10s` | Ping interval over the encrypted control stream (Go duration). |
| `livenessTimeout` | string | `5s` | Pong wait before counting a miss. |
| `livenessFailures` | int | `3` | Missed pongs before the smux session is rebuilt. |
| `maxSessionDuration` | string | never | Planned carrier rebuild after N (e.g. `6h`); empty = never. |

Liveness probes the smux **control stream after the handshake**, not just the
WebRTC/provider status: if pongs stop, the session is rebuilt (and the carrier
told to reconnect). Use the same liveness/lifecycle values on both sides.

### Client‑only identity (outbound)

| Field | Type | Default | Notes |
|-------|------|---------|-------|
| `uuid` | string | — | The subscription this client belongs to. The inbound resolves it against its user list — see [Keys & identity](#keys--identity). |
| `deviceId` | string | random | Which **machine** this is. Device caps and the fair split between the people sharing a subscription are counted over it. |
| `deviceIdPath` | string | — | File to persist an auto‑generated device id across restarts (ignored when `deviceId` is set). |

---

## Providers

`provider` selects the disguise service and how session credentials are obtained.

| Provider | Underlying engine | Room / auth | Notes |
|----------|-------------------|-------------|-------|
| **`telemost`** | goolom | Room ID from Yandex Telemost | Only **`vp8channel`** is stable. |
| **`wbstream`** | livekit | Room ID from stream.wb.ru; optional `authToken` | Guest tokens carry `canPublishData=false`. Use `vp8channel`/`seichannel`/`videochannel`. |
| **`none`** | set by `engine` | `url` + `token` | Direct engine mode; bypass the provider auth flow and talk to an SFU directly. |

> Always confirm the SFU service you pick is reachable/allowed on your network.

---

## Transports & speed

`transport` decides how tunnel bytes are placed into a WebRTC primitive.

| Transport | How it carries data | Accepted here |
|-----------|---------------------|---------------|
| **`vp8channel`** | KCP over VP8‑like video frames | ✅ |
| **`seichannel`** | Payload in H.264 SEI NAL units, with ACK/retry | ✅ |
| **`videochannel`** | Bytes rendered as QR/tile frames via ffmpeg, with ACK/retry | ✗ — needs `ffmpeg`, which the runtime image does not carry |
| **`datachannel`** | A WebRTC data channel | ✗ — neither provider offers one |

The last two are rejected by [`infra/conf/olcrtc.go`](../../infra/conf/olcrtc.go)
when a config names them, so a broken carrier fails at startup with a clear
message rather than somewhere inside a provider handshake. Their code is still
compiled in: excluding it saves 1.3 MB of a 50 MB binary and would cost a
rewrite of upstream's validation tests, which is a bad trade for a fork that has
to resync. `supportedTransports` in that file is the one place to change if you
want them back.

### Compatibility matrix

Which transport works on which provider (from olcRTC's E2E suite):

| Transport | telemost | wbstream |
|-----------|:--------:|:--------:|
| `vp8channel` | ✅ | ✅ |
| `seichannel` | ✗ | ✅ |
| `videochannel` | ✅ | ✅ |

✅ works · ~ unstable (may work) · ✗ not supported.

### Relative speed & server cost

olcRTC ranks throughput strictly as:

> **`vp8channel` > `seichannel` > `videochannel`**

Absolute numbers depend heavily on the **SFU, the network path, and CPU**, so
treat the bands below as order‑of‑magnitude guidance, not guarantees:

| Transport | Rough throughput\* | Server CPU / RAM | Why |
|-----------|--------------------|------------------|-----|
| `vp8channel` | A few Mbit/s (scales with `fps`×`batchSize` and the SFU's video bitrate) | Medium | KCP framing + VP8‑style pacing. |
| `seichannel` | Below vp8channel | Medium–high | Data embedded in an H.264 stream + ACK/retry overhead. |
| `videochannel` | Lowest — often sub‑Mbit/s | **Highest** (spawns ffmpeg) | Bytes go through image (QR/tile) encode/decode. Experimental. |

\* Per tunnel/session and workload‑dependent.

**Tuning knobs that trade CPU for speed** (video transports): raise `batchSize`
and `bitrate` for more throughput; lower `fps` to cut CPU. `videochannel` is the
most CPU‑hungry because of ffmpeg and is best treated as a fallback.

**Recommended:** start with **`wbstream + vp8channel`**; for **Telemost** use
**`vp8channel`**.

---

## Keys & identity

### Why the shared key had to go

The tunnel used to be encrypted with one XChaCha20 key per room, held by every
client. That had three consequences:

- a key leaked by one client compromised every other;
- rotating it meant reissuing every client's configuration at once;
- and — the one that mattered most — **everyone in the room could decrypt
  everyone else's traffic**, which ruled out the cheap way to serve several
  users: putting them in one room rather than one room each.

### What replaces it

The server holds a long‑term X25519 key pair. Generate one with the command
Xray already ships:

```bash
xray x25519
# Private key: <goes in the inbound's privateKey>
# Public key:  <goes in every client's publicKey>
```

Before anything else flows, two frames establish a key for that connection
alone:

```
client                                        server
  │  e_pub ‖ AEAD(k_init){version, timestamp}  │
  │ ─────────────────────────────────────────► │  only the holder of the
  │                                            │  private key can open this
  │  s_epub ‖ AEAD(k_reply){version}           │
  │ ◄───────────────────────────────────────── │
  ▼ both derive k_session; everything after uses it
```

To anyone else in the room both frames are indistinguishable from random: they
cannot derive `k_init` without the server's private key, so they can neither
read the exchange nor answer it. That is the same protection against stray
peers the room key used to give, without the key being a secret anyone has to
distribute.

Compromising the server's private key later does not open recorded sessions:
that yields one of the two shared secrets, and the session key needs both — the
other requires an ephemeral private key both sides discard.

### Two identities, and why they are separate

| | What it says | What it is used for |
|---|---|---|
| `uuid` | **which subscription** | authorisation, per‑user traffic, quotas, expiry |
| `deviceId` | **which machine** | device caps, the fair split between the people sharing a subscription |

Both travel inside the handshake on the first smux stream, which by then is
already encrypted with the session key — so neither is recoverable from
recorded traffic even by someone who later obtains the server's private key.
A 0‑RTT design that put the credential in the opening frame would lose exactly
that.

With no users registered the inbound is **open**: anyone who completes the key
exchange gets in, which is the right behaviour for a server whose panel has not
provisioned it yet. Once a single user exists, the list is enforced and an
unrecognised `uuid` is refused.

### Add / remove users at runtime (no restart)

Same API calls as any other protocol, against the olcrtc inbound's **tag**:

```bash
xray api adu --server=127.0.0.1:10085 add_user.json           # add
xray api rmu --server=127.0.0.1:10085 -tag="olcrtc-in" "<uuid>"   # remove
```

The inbound resolves a client's `uuid` against the **label** its users were
registered under, so provision users with that label set to the subscription's
UUID — which is what panels that key users by UUID already do. The Xray API
requires *some* account on a user, so include a placeholder; olcrtc ignores it.

```json
{ "inbounds": [ { "tag": "olcrtc-in", "protocol": "trojan",
  "settings": { "clients": [ { "email": "<uuid>", "password": "placeholder" } ] } } ] }
```

Removing a user refuses their next handshake. Connections already open keep
running until their carrier is rebuilt; remove the user from the tariff store
too if you need them cut off immediately.

---

## Speed limiting

Per-user traffic shaping is enforced in the dispatcher and therefore applies to
olcrtc exactly as it does to VLESS and Hysteria — the same plan, aggregate
across every protocol and all of a user's devices, keyed by **email**.

It is more than a cap: a fair split between the devices sharing a subscription,
a speed boost that makes the first minute of a session feel unmetered, and a
priority allowance that keeps DNS lookups and TLS handshakes responsive while a
download is saturating the plan.

Plans are keyed by the user's **Xray level**, so an external panel selects one
simply by creating the user at that level — no extra API, no restart.

See [`common/shaper/README.md`](../../common/shaper/README.md) for the tariff
table, the tuning knobs, the cost, and how to replace the level table with a
live policy store.

> **Note on device identity.** For socket inbounds a device is its source IP.
> olcrtc traffic currently arrives with no per-device address of its own, so
> all of a user's olcrtc connections count as one device and share one slot in
> the fair split. Real per-device identity for olcrtc is a
> [roadmap](#roadmap--planned-follow-ups) item, tied to per-user secrets.

## Connected users

Enable stats + online tracking and olcrtc users appear alongside every other
protocol's:

```json
"stats": {},
"policy": { "levels": { "0": {
  "statsUserUplink": true, "statsUserDownlink": true, "statsUserOnline": true } } }
```

Then, keyed by email:

```bash
xray api statsgetallonlineusers --server=127.0.0.1:10085   # who is connected
xray api statsonline --server=127.0.0.1:10085 -email "alice@myapp"
xray api statsquery  --server=127.0.0.1:10085 -pattern "user>>>alice@myapp"
```

There is no push API — poll `GetAllOnlineUsers` and diff the set to derive
connect/disconnect events (see the top‑level integration notes). Per‑**device**
granularity for olcrtc is a [roadmap](#roadmap--planned-follow-ups) item
(olcrtc traffic has no per‑device client IP of its own).

---

## Example configs

Validate any config with `xray run -test -c config.json`. Minimal ready‑to‑edit
files live in [`example/client.json`](example/client.json) and
[`example/server.json`](example/server.json).

### Client outbound (wbstream + vp8channel, recommended)

```json
{
  "log": { "loglevel": "info" },
  "inbounds": [
    { "tag": "socks-in", "listen": "127.0.0.1", "port": 1080, "protocol": "socks",
      "settings": { "udp": false },
      "sniffing": { "enabled": true, "destOverride": ["http", "tls"] } }
  ],
  "outbounds": [
    { "tag": "olcrtc-out", "protocol": "olcrtc", "settings": {
        "provider": "wbstream", "transport": "vp8channel",
        "roomId": "https://meet.small-dm.ru/REPLACE_ROOM",
        "publicKey": "REPLACE_WITH_THE_SERVER_PUBLIC_KEY",
        "uuid": "REPLACE_WITH_SUBSCRIPTION_UUID",
        "deviceId": "alice-laptop",
        "dnsServer": "8.8.8.8:53"
    } },
    { "tag": "direct", "protocol": "freedom" }
  ],
  "routing": { "rules": [
    { "type": "field", "ip": ["geoip:private"], "outboundTag": "direct" }
  ] }
}
```

### Server: multi‑protocol (hysteria + vless + olcrtc) with the management API

One server exposing several protocols, per‑user speed limits, online tracking,
and the gRPC API your app drives:

```json
{
  "log": { "loglevel": "warning" },

  "api": { "tag": "api", "services": ["HandlerService", "StatsService", "LoggerService"] },
  "stats": {},
  "policy": {
    "levels": { "0": { "statsUserUplink": true, "statsUserDownlink": true, "statsUserOnline": true } },
    "system": { "statsInboundUplink": true, "statsInboundDownlink": true }
  },

  "inbounds": [
    { "listen": "127.0.0.1", "port": 10085, "protocol": "dokodemo-door",
      "settings": { "address": "127.0.0.1" }, "tag": "api" },

    { "tag": "vless-in", "listen": "0.0.0.0", "port": 443, "protocol": "vless",
      "settings": { "clients": [], "decryption": "none" },
      "streamSettings": { "network": "tcp" } },

    { "tag": "hy-in", "listen": "0.0.0.0", "port": 8443, "protocol": "hysteria",
      "settings": { "users": [] } },

    { "tag": "olcrtc-in", "protocol": "olcrtc", "settings": {
        "provider": "wbstream", "transport": "vp8channel",
        "roomId": "https://meet.small-dm.ru/REPLACE_ROOM",
        "privateKey": "REPLACE_WITH_THE_SERVER_PRIVATE_KEY",
        "dnsServer": "8.8.8.8:53"
    } }
  ],

  "outbounds": [ { "tag": "direct", "protocol": "freedom" } ],

  "routing": { "rules": [
    { "type": "field", "inboundTag": ["api"], "outboundTag": "api" }
  ] }
}
```

Users are added at runtime per inbound tag (`vless-in`, `hy-in`, `olcrtc-in`)
via `xray api adu` — see [Keys & identity](#keys--identity). The per‑user
speed limit and online tracking apply uniformly across all three.

### Server: wbstream + vp8channel (guest flow, no data‑channel rights)

```json
{
  "inbounds": [ { "tag": "olcrtc-in", "protocol": "olcrtc", "settings": {
      "provider": "wbstream", "transport": "vp8channel",
      "roomId": "REPLACE_ROOM_FROM_stream.wb.ru",
      "privateKey": "REPLACE_WITH_THE_SERVER_PRIVATE_KEY",
      "dnsServer": "8.8.8.8:53",
      "authToken": "OPTIONAL_WBSTREAM_TOKEN",
      "vp8": { "fps": 30, "batchSize": 64 }
  } } ],
  "outbounds": [ { "tag": "direct", "protocol": "freedom" } ]
}
```

The matching client is identical except `protocol` sits under `outbounds[]` and
you add a local `socks`/`http` inbound plus (optionally) `deviceId`.

---

## Embedded usage (xray‑core as a library)

Build the proxy settings as a `TypedMessage` and hand them to `core.Config`.
Blank‑import the proxy (and proxyman/router/dispatcher) so the config types and
the self‑driven inbound handler register.

```go
import (
    "github.com/xtls/xray-core/common/serial"
    "github.com/xtls/xray-core/core"
    "github.com/xtls/xray-core/proxy/olcrtc"

    _ "github.com/xtls/xray-core/main/distro/all" // registers everything, incl. olcrtc
)

out := &core.OutboundHandlerConfig{
    Tag: "olcrtc-out",
    ProxySettings: serial.ToTypedMessage(&olcrtc.ClientConfig{
        Provider:  "wbstream",
        Transport: "vp8channel",
        RoomId:    "https://meet.small-dm.ru/my-room",
        PublicKey: serverPublicKey,
        Uuid:      subscriptionUUID,
        DeviceId:  "alice-laptop",
        DnsServer: "8.8.8.8:53",
    }),
}
// add `out` to core.Config.Outbound, plus a socks/dokodemo inbound, then core.New(cfg).
```

For a server, use `&olcrtc.ServerConfig{...}` in an `InboundHandlerConfig`
(no `ReceiverConfig` port needed) plus a `freedom` outbound for egress. The
lower‑level olcrtc library is vendored under [`olcrtclib/`](olcrtclib) and
exposed through olcRTC's own embedding API under
[`olcrtclib/pkg/olcrtc`](olcrtclib/pkg/olcrtc) — `tunnel.NewWithDial` for a
server whose egress you supply, `client.StartTunnel` for a carrier you dial
over yourself — if you want to drive the tunnel directly.

---

## Roadmap / planned follow‑ups

Both of the entries that used to be here are done: per‑user credentials and
per‑device counting, and a key that is negotiated rather than shared. See
[Keys & identity](#keys--identity).

What is left:

- **Room health and failover.** The inbound joins one room and stays there. A
  room that is blocked or that the provider retires takes the tunnel with it.
  Wanted: a pool of rooms, a prober that tells `healthy` from `degraded` from
  `blocked` (provider API answers but no media flows — the shape a DPI block
  takes), hot standbys, and the status pushed to the panel so it can reissue
  subscriptions.
- **Several users per room in practice.** The cryptography no longer stands in
  the way, and per‑peer routing exists on `vp8channel`. What has not been
  measured is how many peers one room carries before the SFU itself becomes the
  limit.
- **A post‑quantum layer.** The exchange reserves a version byte inside its
  encrypted payload precisely so ML‑KEM‑768 can be added alongside X25519
  without the frame layout changing. Xray already vendors the primitive.

---

## Caveats

- **TCP only.** olcrtc tunnels stream (TCP) connections; the outbound rejects
  UDP targets. Use another outbound for UDP if needed.
- **Server DNS.** `dnsServer` reaches the SFU. Provider auth API calls
  (Telemost/WbStream) use the host's system resolver; Xray's global DNS is not
  overridden by this proxy.
- **`videochannel` needs `ffmpeg`** on the host (and `codec:"tile"` needs
  1080×1080).
- The `example/*.json` demo room `meet.small-dm.ru` may be down; substitute a
  Telemost/WbStream endpoint that works on your network.

---

## Keeping the library in step

`olcrtclib` is a copy of [olcRTC](https://github.com/openlibrecommunity/olcrtc)
with its import paths rewritten onto this module, and
[`fork/bin/olcrtc`](../../fork/bin/olcrtc) is what keeps it honest:

```bash
./fork/bin/olcrtc diff   ~/src/olcrtc   # what has moved upstream
./fork/bin/olcrtc vendor ~/src/olcrtc   # bring it over, patches and all
./fork/bin/olcrtc export ~/src/olcrtc   # regenerate patches after editing
```

The split is the same one `fork/bin/fork` uses for Xray itself: what the fork
adds lives in **its own files** (listed in `fork/olcrtc-manifest.txt`), and the
handful of lines it needs **inside** upstream's are patches. There are currently
five, about a hundred lines, and three of them only exist because jitsi is not
vendored.

Nothing watched this before, and by the time anyone looked 41 of 43 vendored
files had changed upstream and 8 were gone — including a rewrite of the crypto
and a refactor that moved most of the client and server into a new package. Run
`olcrtc diff` before assuming the copy is current.

---

## How it's wired (for maintainers)

- `olcrtclib/` — vendored olcrtc library (import paths rewritten to this module).
  `internal/client` exposes `StartTunnel`/`Tunnel.DialContext`; `internal/server`
  exposes a `DialHook` (egress delegated to Xray's dispatcher) and an `AuthHook`
  that receives the client `deviceId`; the `DialFunc` carries the authenticated
  `sessionID`. `olcrtclib/pkg/olcrtc` is the only surface `proxy/olcrtc`
  depends on; everything under `olcrtclib/internal` is out of reach by Go's own
  rules, which is what keeps the seam honest.
- User system: [`validator.go`](validator.go) (sync.Map store) + `Server`
  implements `proxy.UserManager` and stub `proxy.Inbound`; the authenticated
  identity is encoded into the sessionID and decoded in `dispatch` to set
  `session.Inbound.User`. `app/proxyman/inbound/selfdriven.go` gained
  `GetInbound()` so `AlterInbound` can reach the UserManager.
- Speed limit: [`app/dispatcher/ratelimit.go`](../../app/dispatcher/ratelimit.go)
  wraps the per‑user link writers in `getLink` (keyed by email).
- `config.pb.go` — generated offline (no protoc) from `config.proto` via
  `protoc-gen-go` driven by a hand‑built `FileDescriptorProto`.
- Self‑driven inbound seam: `proxy.RegisterSelfDrivenInbound` +
  `app/proxyman/inbound/selfdriven.go`; `NewHandler` routes olcrtc there and
  `infra/conf` skips the port requirement for it.
