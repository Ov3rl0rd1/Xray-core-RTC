# olcRTC proxy for Xray

`proxy/olcrtc` integrates the [olcRTC](https://github.com/openlibrecommunity/olcrtc)
encrypted TCP‑over‑WebRTC tunnel into this Xray fork as a first‑class proxy
protocol, with **both** a client (outbound) and a server (inbound).

Traffic is disguised as an ordinary video call on an allowed SFU service
(Yandex Telemost, WbStream) and additionally encrypted end‑to‑end
with a shared XChaCha20‑Poly1305 key. Inside the call it multiplexes many TCP
connections (smux) over the WebRTC data/video channel.

```
app ─▶ Xray (socks/vless/…) ─▶ olcrtc outbound  ══WebRTC/SFU══▶  olcrtc inbound ─▶ Xray router ─▶ internet
                                (client / cnc)                    (server / srv)
```

**Contents:** [Roles](#roles) · [Settings reference](#settings-reference) ·
[Providers](#providers) · [Transports & speed](#transports--speed) ·
[Users & identity](#users--identity) · [Speed limiting](#speed-limiting) ·
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

Both sides must share the same **`key`** and **`roomId`** and use the same
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
| `transport` | string | ✅ | — | `vp8channel`, `seichannel`, `videochannel`. See [Transports](#transports--speed). |
| `roomId` | string | ✅¹ | — | Room reference for the provider. ¹Required unless `provider:"none"`. Telemost/WbStream: room ID created on the service site. |
| `key` | string | ✅ | — | 64 hex chars (32‑byte shared key). `openssl rand -hex 32`. **Identical on both sides.** |
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

### `video` object — videochannel tuning (needs `ffmpeg`)

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
| `deviceId` | string | random | Identity echoed to the server's auth hook. **With users configured this is the user's login** — see [Users & identity](#users--identity). |
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

| Transport | How it carries data | Needs |
|-----------|---------------------|-------|
| **`vp8channel`** | KCP over VP8‑like video frames | — |
| **`seichannel`** | Payload in H.264 SEI NAL units, with ACK/retry | — |
| **`videochannel`** | Bytes rendered as QR/tile frames via ffmpeg, with ACK/retry | `ffmpeg` (`tile` ⇒ 1080×1080) |

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

## Users & identity

olcrtc participates in Xray's user system like VLESS/Trojan, so users are managed
the same way from an external app (typically the gRPC `HandlerService`).

**Model.** The shared `key` (room key) is the *cryptographic* gate — every
legitimate client holds it. On top of that, an olcrtc **user** is an
allow‑listed identity used for authorization, per‑user stats, speed limits and
revocation. The client presents its identity as the outbound's **`deviceId`**;
the inbound validates it:

- **No users registered → open mode:** any client with the room key is admitted
  (pre‑user‑auth behaviour).
- **≥1 user registered → allow‑list enforced:** `deviceId` must equal a
  registered user's email, or the handshake is rejected.

Set the identity on the client outbound:

```json
"settings": { "provider": "wbstream", "transport": "vp8channel",
  "roomId": "https://meet.example/room", "key": "<64 hex>",
  "deviceId": "alice@myapp" }
```

### Add / remove users at runtime (no restart)

Same API calls as any other protocol, against the olcrtc inbound's **tag**:

```bash
xray api adu --server=127.0.0.1:10085 add_user.json           # add
xray api rmu --server=127.0.0.1:10085 -tag="olcrtc-in" "alice@myapp"   # remove
```

olcrtc keys users by **email only** and ignores the account, but the Xray API
requires *some* registered account on the user, so include a placeholder
(`add_user.json`, any value — it is not used):

```json
{ "inbounds": [ { "tag": "olcrtc-in", "protocol": "trojan",
  "settings": { "clients": [ { "email": "alice@myapp", "password": "placeholder" } ] } } ] }
```

Or call `HandlerService.AlterInbound` → `AddUserOperation` directly with
`tag:"olcrtc-in"` and a placeholder account. Removing a user drops future
handshakes for that identity.

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
        "key": "REPLACE_WITH_64_HEX",
        "dnsServer": "8.8.8.8:53",
        "deviceId": "alice@myapp"
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
        "key": "REPLACE_WITH_64_HEX",
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
via `xray api adu` — see [Users & identity](#users--identity). The per‑user
speed limit and online tracking apply uniformly across all three.

### Server: wbstream + vp8channel (guest flow, no data‑channel rights)

```json
{
  "inbounds": [ { "tag": "olcrtc-in", "protocol": "olcrtc", "settings": {
      "provider": "wbstream", "transport": "vp8channel",
      "roomId": "REPLACE_ROOM_FROM_stream.wb.ru",
      "key": "REPLACE_WITH_64_HEX",
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
        Key:       key64hex,
        DnsServer: "8.8.8.8:53",
        DeviceId:  "alice@myapp",
    }),
}
// add `out` to core.Config.Outbound, plus a socks/dokodemo inbound, then core.New(cfg).
```

For a server, use `&olcrtc.ServerConfig{...}` in an `InboundHandlerConfig`
(no `ReceiverConfig` port needed) plus a `freedom` outbound for egress. The
lower‑level olcrtc library is vendored under [`olcrtclib/`](olcrtclib) and
exposed through [`olcrtclib/bridge`](olcrtclib/bridge) (`bridge.StartClient` /
`bridge.RunServer`) if you want to drive the tunnel directly.

---

## Roadmap / planned follow‑ups

- **Per‑user secret tokens** (instead of email‑as‑identity) and per‑device
  counting: needs an olcrtc `Account` proto message + a client `userToken` field.
  Straightforward given the offline `.pb.go` pipeline noted below.
- **Server‑generated room key delivered via asymmetric crypto.** The room key
  must be identical on both sides *before* any handshake (it encrypts the whole
  channel), so it is provisioned **out‑of‑band** by your app, not negotiated
  inside the tunnel. Recommended flow: the client generates a keypair and sends
  its public key to your API; the server generates the room key once and returns
  it **sealed to that public key**; the client unseals it into the outbound
  `key`. A distinct room key *per user* would need per‑peer ciphers / trial
  decryption in the transport — a larger change.

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

## How it's wired (for maintainers)

- `olcrtclib/` — vendored olcrtc library (import paths rewritten to this module).
  `internal/client` exposes `StartTunnel`/`Tunnel.DialContext`; `internal/server`
  exposes a `DialHook` (egress delegated to Xray's dispatcher) and an `AuthHook`
  that receives the client `deviceId`; the `DialFunc` carries the authenticated
  `sessionID`. `bridge/` is the only exported surface `proxy/olcrtc` depends on.
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
