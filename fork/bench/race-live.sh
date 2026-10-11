#!/bin/bash
# race-live.sh — race-detector run of the live-control surface: list/close/reload/swap under
# game + long + churn load, with the instance restarted every 7 s.
. "$(dirname "$0")/env.sh"
cd "$BENCH"
rm -f race.*; pkill -x harness-race; rm -f harness.fifo; mkfifo harness.fifo
ip netns exec hcli env GORACE="log_path=$BENCH/race" ./harness-race -c proc-hy.json -restart 7s -stats 1h < harness.fifo > harness.out 2>&1 &
HP=$!
exec 3>harness.fifo; sleep 2
ip netns exec hcli ./load -games 2 -long 4 -churn 8 -d 30s -r 30s > load.out 2>&1 &
for i in $(seq 1 25); do
  kill -0 $HP 2>/dev/null || { echo "HARNESS DIED at step $i"; break; }
  echo conns >&3; echo "close $((i*3)),$((i*3+1))" >&3
  [ $((i%5)) -eq 0 ] && echo "reload $BENCH/routing-load-direct.json" >&3
  [ $((i%7)) -eq 0 ] && echo "swap $BENCH/ob-vless.json" >&3
  sleep 1
done
sleep 6; exec 3>&-; kill -INT $HP 2>/dev/null; sleep 2
echo "commands answered: $(grep -c '^conns\|^closed\|^reload\|^swap' harness.out)"
grep -m3 "^fatal\|^panic" harness.out
for f in race.*; do [ -f "$f" ] || continue; echo "$f: $(grep -c 'DATA RACE' "$f") races"
  grep -A4 "DATA RACE" "$f" | grep -oE "(fork_[a-z_]+|lib_live|live|session/fork_routed)\.go:[0-9]+" | sort | uniq -c; done
