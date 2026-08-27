#!/usr/bin/env bash
# G2-sched-micros (v4 plan): PingPongHog / CreateGoroutines{,Parallel,Capture}
# at GOMAXPROCS=256, experiment-off vs experiment-on runtime test binaries,
# n interleaved rounds, benchstat-ready raw files.
#
# Runs ON numa-dell from the tree at /home/deparker/go-numa. Build the two
# test binaries first (see v4 plan Task 7):
#   cd /home/deparker/go-numa/src
#   GOROOT=/home/deparker/go-numa ../bin/go test -c -o /tmp/g2/rt-off.test runtime
#   GOROOT=/home/deparker/go-numa GOEXPERIMENT=numa ../bin/go test -c -o /tmp/g2/rt-on.test runtime
set -euo pipefail
OFF="${OFF:-/tmp/g2/rt-off.test}"
ON="${ON:-/tmp/g2/rt-on.test}"
OUT="${OUT:-$HOME/v4-g2-sched}"
N="${N:-10}"
BENCH='PingPongHog|CreateGoroutines$|CreateGoroutinesParallel|CreateGoroutinesCapture'
mkdir -p "$OUT"
busy=$(ps -eo pcpu,comm --sort=-pcpu | awk 'NR>1 && $2 != "ps" && $2 != "awk" {print int($1), $2; exit}' | awk '$1>50{print $2}')
[ -z "$busy" ] || { echo "machine not idle: $busy"; exit 1; }
[ "$(cat /proc/sys/kernel/numa_balancing)" = "1" ] || { echo "numa_balancing != 1"; exit 1; }
: >"$OUT/off.out"; : >"$OUT/on.out"
for round in $(seq "$N"); do
	echo "== round $round =="
	if [ $((round % 2)) -eq 1 ]; then order="off on"; else order="on off"; fi
	for arm in $order; do
		case "$arm" in
			off) bin="$OFF" ;;
			on)  bin="$ON" ;;
		esac
		env GOMAXPROCS=256 "$bin" -test.run '^$' -test.bench "$BENCH" -test.benchtime 1s \
			>>"$OUT/$arm.out" 2>>"$OUT/$arm.log"
	done
done
echo "done; benchstat $OUT/off.out $OUT/on.out"
