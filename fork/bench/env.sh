# Sourced by every bench script. BENCH is where binaries, keys, rendered configs and logs
# live; the scripts themselves stay in fork/bench.
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
BENCH=${BENCH:-/tmp/xray-bench}
