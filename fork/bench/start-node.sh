#!/bin/bash
# start-node.sh — the far side: echo/HTTP/TLS/DNS/game servers on 10.99.1.1, and the
# VLESS-REALITY (tcp/443) + Hysteria2 (udp/8443, salamander) node on 10.99.0.1.
. "$(dirname "$0")/env.sh"
cd "$BENCH"
pkill -x servers 2>/dev/null; pkill -f "xray run -c $BENCH/server.json" 2>/dev/null; sleep 0.3
(ip netns exec hsrv ./servers -bind 10.99.1.1 -cert dest.crt -key dest.key > servers.log 2>&1 &)
(ip netns exec hsrv ./xray run -c "$BENCH/server.json" > server.out 2>&1 &)
sleep 1.5
ip netns exec hsrv ss -ltnup | awk 'NR==1 || /10\.99/'
