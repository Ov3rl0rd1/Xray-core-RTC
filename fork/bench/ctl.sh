#!/bin/bash
# ctl.sh start <config> | send <command...> | stop — a long-running harness driven through a
# fifo, for poking at the live API by hand: conns | close <ids|*> | reload <file> | swap <file> | restart | stats
. "$(dirname "$0")/env.sh"
F=$BENCH/harness.fifo
case $1 in
start) pkill -x harness-bin; rm -f "$F"; mkfifo "$F"
       (ip netns exec hcli "$BENCH/harness-bin" -c "$2" -stats 1h < "$F" > "$BENCH/harness.out" 2>&1 &)
       exec 3>"$F"; sleep 1.5; echo started;;
send)  shift; echo "$*" > "$F"; sleep 0.5; tail -n ${TAILN:-2} "$BENCH/harness.out";;
stop)  pkill -INT -x harness-bin;;
esac
