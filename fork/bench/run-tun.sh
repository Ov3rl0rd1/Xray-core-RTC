#!/bin/bash
# run-tun.sh <client-config> <load args...> — the core's own TUN inbound, as the Windows
# client runs it; load uses plain sockets, so it measures whatever the route table does.
. "$(dirname "$0")/env.sh"
CFG=$1; shift
HARN=${HARN:-$BENCH/harness-bin}
pkill -x harness-bin 2>/dev/null; pkill -x harness-race 2>/dev/null; sleep 0.3
ip netns exec hcli env GORACE="log_path=$BENCH/race halt_on_error=0" $HARN -c "$CFG" ${HARGS:-} > "$BENCH/harness.out" 2>&1 &
for i in $(seq 1 50); do ip netns exec hcli ip route show | grep -q xtun0 && break; sleep 0.1; done
ip netns exec hcli ip route show | grep xtun0 | tr '\n' ';'; echo
ip netns exec hcli "$BENCH/load" "$@"
pkill -INT -x harness-bin; pkill -INT -x harness-race; sleep 1.5
echo "--- harness"; grep -v "^t=" "$BENCH/harness.out" | grep -v deprecated | tail -5; grep "^t=" "$BENCH/harness.out" | tail -2
