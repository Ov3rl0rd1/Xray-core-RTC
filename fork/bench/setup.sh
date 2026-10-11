#!/bin/bash
# setup.sh — build the bench and render its configs into $BENCH (default /tmp/xray-bench).
# Needs: go, openssl, iproute2 (and root to run the namespaces later).
set -euo pipefail
. "$(dirname "$0")/env.sh"
mkdir -p "$BENCH"
cd "$ROOT"

echo "building into $BENCH"
go build -o "$BENCH/xray" ./main
go build -tags bench -o "$BENCH/servers" ./fork/bench/servers
go build -tags bench -o "$BENCH/load" ./fork/bench/load

# The harness runs libxray's own logic (api.go, live.go) without the cgo shims.
# Tagged like the rest of the bench, so an untagged `go build ./...` never sees them.
for f in api.go live.go; do
  { echo '//go:build bench'; echo; sed -e '/^func main() {}$/d' -e '/^\/\/go:build/d' "libxray/$f"; } > "fork/bench/harness/lib_$f"
done
go build -tags bench -o "$BENCH/harness-bin" ./fork/bench/harness
if [ "${RACE:-1}" = 1 ]; then
  # checkptr off: upstream VLESS Vision converts a stored uintptr back to a pointer and
  # trips it on the first connection. Not a production crash (see README).
  go build -tags bench -race -gcflags=all=-d=checkptr=0 -o "$BENCH/harness-race" ./fork/bench/harness
fi

cd "$BENCH"
if [ ! -f uuid.txt ]; then
  ./xray uuid > uuid.txt
  ./xray x25519 > x25519.txt
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout node.key -out node.crt \
    -days 30 -subj "/CN=node.test" 2>/dev/null
  openssl x509 -in node.crt -outform der | sha256sum | cut -d' ' -f1 > node.sha
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout dest.key -out dest.crt \
    -days 30 -subj "/CN=www.example.com" -addext "subjectAltName=DNS:www.example.com" 2>/dev/null
fi
UUID=$(cat uuid.txt)
PRIV=$(grep PrivateKey x25519.txt | awk '{print $2}')
PUB=$(grep PublicKey x25519.txt | awk '{print $NF}')
SHA=$(cat node.sha)
for t in "$HERE"/templates/*.json; do
  sed -e "s#@DIR@#$BENCH#g" -e "s#@UUID@#$UUID#g" -e "s#@PRIV@#$PRIV#g" -e "s#@PUB@#$PUB#g" -e "s#@SHA@#$SHA#g" \
    "$t" > "$BENCH/$(basename "$t")"
done
echo "ready: $(ls "$BENCH" | tr '\n' ' ')"
