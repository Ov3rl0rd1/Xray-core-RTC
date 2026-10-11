# Bench: the core under a player's traffic, on one Linux box

Two network namespaces stand in for a player's PC and the internet, the real core runs in
between exactly as the Windows client embeds it (libxray's own `api.go`/`live.go`, in-process),
and a load generator produces what a gamer's machine produces: a 60 Hz UDP game stream,
long-lived TCP sessions, connection churn and bulk downloads. It reports what a player would
notice — stalls on the game stream, broken long sessions, failed connects — not averages.

It exists because "improve logging" never answered why a tunnel dropped mid-match. Each
finding below came out of this bench, before any code was changed.

```
 hcli (player's PC)                        hsrv (node + "internet")
 10.99.0.2 ── veth ── 10.99.0.1  VLESS-REALITY tcp/443, Hysteria2 udp/8443 (salamander)
                      10.99.1.1  echo :7, http :80, TLS :443 (REALITY target), DNS :53,
                                 game server udp/27015, discard :9, chargen :19
```

## Setup

Needs root (namespaces, TUN), Go, openssl, iproute2, iptables.

```bash
export BENCH=/tmp/xray-bench           # binaries, keys, rendered configs, logs
./fork/bench/setup.sh                  # builds xray, servers, load, harness (+race); renders configs
./fork/bench/netns-up.sh               # DELAY=30ms JITTER=5ms LOSS=1% for a bad link
./fork/bench/start-node.sh             # node + target servers in hsrv
```

`setup.sh` is idempotent; keys and certificates are generated once per `$BENCH` and are bench-only.
Everything Go here builds only with `-tags bench`, so `go build ./...` never sees it.

## Scenarios

| Script | What it answers |
|---|---|
| `run-socks.sh $BENCH/client-hy.json -games 2 -long 4 -churn 8 -d 30s` | Is the core itself sound, without any TUN? (SOCKS5 CONNECT + UDP ASSOCIATE) |
| `run-tun.sh $BENCH/client-tun-hy.json …` | The core's own TUN inbound, as the Windows client runs it. `load` uses plain sockets, so it measures whatever the route table does. |
| `HARGS="-reset 10s" run-tun.sh …` | What a session reset costs (the old Windows network monitor did one on every adapter change). |
| `HARGS="-restart 7s" run-tun.sh …` | What a full core restart costs (the old recovery path). |
| `chaos-udpblock.sh 10 8 & run-tun.sh $BENCH/client-tun-hy.json -d 40s` | An ISP throttling QUIC for 8 s: does Hysteria2 come back by itself? |
| `race-live.sh` | Race detector over the live-control API (list/close/reload/swap) under load, with the instance restarted every 7 s. |
| `ctl.sh start $BENCH/proc-hy.json`, `ctl.sh send conns` … | Poke the live API by hand: `conns`, `close <ids\|*>`, `reload <file>`, `swap <file>`, `restart`, `stats`. |

Config variants in `templates/`: `client-*` (SOCKS-only), `client-tun-*` (TUN), `proc-hy`
(TUN with process routing), `failclosed` (no physical interface to bind to), `mtu` (MTU 9000),
`ob-*` (outbounds for `swap`), `routing-load-direct` (rules for `reload`).

## Reading the output

`load` prints one line per interval and a `FINAL` line:

```
game sent=479 recv=957 rtt p50=800µs p99=2.1ms max=4.9ms stalls=0 maxgap=20ms err=0 | long breaks=0 | churn ok=304 fail=0 | dl=…
```

`stalls` (gaps over 500 ms on the game stream) and `long breaks` are what kick a player from a
server. The harness prints `t=… goroutines heap controllers restarts resets` — a `controllers`
count that grows across restarts is a leak (it did, before `fork_bind.go`).

## What it found (Oct 2026)

- A session reset on Hysteria2 drops every TCP session it carries (4 resets → 16/16 long
  sessions broken). The Windows client reset on any adapter change; it no longer does.
- The TUN's outbound-binding controller was registered once per start and never removed
  (7 after 6 restarts). Fixed in `proxy/tun/fork_bind.go`.
- The gVisor stack attached its NIC before the TCP/UDP handlers existed: 47–54 data races per
  `race-live.sh` run, all in that start window, and packets in it were answered by gVisor
  itself (RST / ICMP unreachable). Fixed in `proxy/tun/fork_stack_order.go`: 0–1 per run, the
  one left being upstream Hysteria2's `udpSessionManager.closed` (benign).
- An 8 s UDP block heals by itself about 2 s after it lifts; nothing for the client to do.
- MTU 9000 on the TUN: 2.4× bulk throughput (493 vs 206 MB/s) but game p99 under a parallel
  download 12 ms vs 7.9 ms — hence 1500 by default and 9000 as an opt-in "speed mode".

The race build disables `checkptr`: upstream VLESS Vision stores a `uintptr` and converts it
back, which `checkptr` rejects on the first Vision connection. Not a production crash (the
object stays alive and Go's GC does not move it), but it would stop every race run at once.

## What it cannot tell you

Windows itself: wintun, the route table, `IP_UNICAST_IF`, process lookup. That is the job of
`tools/WinTunnelTest` in the Horus repo, which runs the same core on a hosted Windows runner.
