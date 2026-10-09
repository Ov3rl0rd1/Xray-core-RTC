#!/bin/bash
# chaos-udpblock.sh <after-s> <for-s> — drop the node's UDP for a while: an ISP throttling QUIC.
sleep "$1"; ip netns exec hsrv iptables -A INPUT -p udp --dport 8443 -j DROP; echo "[chaos] udp blocked at $(date +%T)"
sleep "$2"; ip netns exec hsrv iptables -D INPUT -p udp --dport 8443 -j DROP; echo "[chaos] udp unblocked at $(date +%T)"
