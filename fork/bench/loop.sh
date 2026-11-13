#!/bin/bash
# loop.sh — a pin that does not hold: does one packet stay one connection?
#
# templates/loop.json routes everything into the TUN and sends everything direct, with no
# autoOutboundsInterface: none of the core's sockets is pinned, so each one follows the
# default route straight back into the TUN. That is what a user's Windows machine did for
# IPv6 (the pin answered WSAEINVAL and the socket was used anyway): one datagram became
# 16 000 sockets in 80 seconds and the machine ran out of ports.
#
# One UDP datagram and one TCP connect are sent; the TUN must take two connections and
# drop every one that comes back from the core's own sockets (proxy/tun/fork_loop.go).
. "$(dirname "$0")/env.sh"
HARN=${HARN:-$BENCH/harness-bin}
LOG=$BENCH/loop.log
pkill -x harness-bin 2>/dev/null; pkill -x harness-race 2>/dev/null; sleep 0.3
: > "$LOG"
ip netns exec hcli "$HARN" -c "$BENCH/loop.json" -d 8s > "$BENCH/harness.out" 2>&1 &
for i in $(seq 1 50); do ip netns exec hcli ip route show | grep -q xtun0 && break; sleep 0.1; done
ip netns exec hcli ip route show | grep xtun0 | tr '\n' ';'; echo

ip netns exec hcli bash -c 'echo -n ping > /dev/udp/10.99.1.1/7' 2>/dev/null
ip netns exec hcli timeout 2 bash -c 'exec 3<>/dev/tcp/10.99.1.1/7' 2>/dev/null
sleep 4

udp=$(grep -c "proxy/tun: processing from udp:.* to udp:10.99.1.1:7" "$LOG")
tcp=$(grep -c "proxy/tun: processing from tcp:.* to tcp:10.99.1.1:7" "$LOG")
dropped=$(grep -c "dropped a connection from the core's own socket" "$LOG")
wait
echo "TUN connections taken: udp=$udp tcp=$tcp; loop warnings: $dropped"
grep "dropped a connection from the core's own socket" "$LOG" | head -2
if [ "$udp" -le 2 ] && [ "$tcp" -le 2 ]; then echo "PASS  one packet stayed one connection"; else echo "FAIL  the core's own sockets came back into the TUN"; exit 1; fi
