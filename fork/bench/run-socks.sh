#!/bin/bash
# run-socks.sh <client-config> <load args...> — the core in-process, load driving its
# SOCKS5 inbound (CONNECT and UDP ASSOCIATE). Isolates the core from any TUN.
. "$(dirname "$0")/env.sh"
CFG=$1; shift
HARN=${HARN:-$BENCH/harness-bin}
pkill -x harness-bin 2>/dev/null; pkill -x harness-race 2>/dev/null; sleep 0.3
ip netns exec hcli env GORACE="log_path=$BENCH/race halt_on_error=0" $HARN -c "$CFG" ${HARGS:-} > "$BENCH/harness.out" 2>&1 &
sleep 1.5
ip netns exec hcli "$BENCH/load" -socks 127.0.0.1:1080 "$@"
pkill -INT -x harness-bin; pkill -INT -x harness-race; sleep 1.5
echo "--- harness"; grep -v "^t=" "$BENCH/harness.out" | tail -5; grep "^t=" "$BENCH/harness.out" | tail -2
