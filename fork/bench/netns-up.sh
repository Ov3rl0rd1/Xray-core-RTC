#!/bin/bash
# netns-up.sh — two namespaces: hcli (the player's PC) and hsrv (the node and "the internet").
# 10.99.0.1 is the node, 10.99.1.1 the targets behind it.
# DELAY=30ms JITTER=5ms LOSS=1% ./netns-up.sh adds netem on both ends of the link.
set -e
ip netns del hcli 2>/dev/null || true
ip netns del hsrv 2>/dev/null || true
ip netns add hcli
ip netns add hsrv
ip link add veth-c type veth peer name veth-s
ip link set veth-c netns hcli
ip link set veth-s netns hsrv
ip -n hsrv addr add 10.99.0.1/24 dev veth-s
ip -n hsrv addr add 10.99.1.1/32 dev lo
ip -n hsrv link set lo up
ip -n hsrv link set veth-s up
ip -n hcli addr add 10.99.0.2/24 dev veth-c
ip -n hcli link set lo up
ip -n hcli link set veth-c up
ip -n hcli route add default via 10.99.0.1
if [ -n "${DELAY:-}" ]; then
  ip netns exec hcli tc qdisc add dev veth-c root netem delay ${DELAY} ${JITTER:-0ms} ${LOSS:+loss $LOSS}
  ip netns exec hsrv tc qdisc add dev veth-s root netem delay ${DELAY} ${JITTER:-0ms} ${LOSS:+loss $LOSS}
fi
echo "netns up (delay=${DELAY:-0})"
